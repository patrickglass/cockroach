// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package spanconfigmanager

import (
	"context"
	"time"

	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/spanconfig"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlutil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
)

// checkReconciliationJobInterval is a cluster setting to control how often we
// check if the span config reconciliation job exists. If it's not found, it
// will be started. It has no effect unless
// spanconfig.experimental_reconciliation.enabled is configured. For host
// tenants, COCKROACH_EXPERIMENTAL_SPAN_CONFIGS needs to be additionally set.
var checkReconciliationJobInterval = settings.RegisterDurationSetting(
	"spanconfig.experimental_reconciliation_job.check_interval",
	"the frequency at which to check if the span config reconciliation job exists (and to start it if not)",
	10*time.Minute,
	settings.NonNegativeDuration,
)

// jobEnabledSetting gates the activation of the span config reconciliation job.
//
// For the host tenant it has no effect unless
// COCKROACH_EXPERIMENTAL_SPAN_CONFIGS is also set.
var jobEnabledSetting = settings.RegisterBoolSetting(
	"spanconfig.experimental_reconciliation_job.enabled",
	"enable the use of the kv accessor", false)

// Manager is the coordinator of the span config subsystem. It ensures that
// there's only one span config reconciliation job for every tenant. It also
// captures all relevant dependencies for the job.
type Manager struct {
	db       *kv.DB
	jr       *jobs.Registry
	ie       sqlutil.InternalExecutor
	stopper  *stop.Stopper
	settings *cluster.Settings
	knobs    *spanconfig.TestingKnobs

	spanconfig.KVAccessor
	spanconfig.SQLWatcherFactory
	spanconfig.SQLTranslator
}

var _ spanconfig.ReconciliationDependencies = &Manager{}

// New constructs a new Manager.
func New(
	db *kv.DB,
	jr *jobs.Registry,
	ie sqlutil.InternalExecutor,
	stopper *stop.Stopper,
	settings *cluster.Settings,
	kvAccessor spanconfig.KVAccessor,
	sqlWatcherFactory spanconfig.SQLWatcherFactory,
	sqlTranslator spanconfig.SQLTranslator,
	knobs *spanconfig.TestingKnobs,
) *Manager {
	if knobs == nil {
		knobs = &spanconfig.TestingKnobs{}
	}
	return &Manager{
		db:                db,
		jr:                jr,
		ie:                ie,
		stopper:           stopper,
		settings:          settings,
		KVAccessor:        kvAccessor,
		SQLWatcherFactory: sqlWatcherFactory,
		SQLTranslator:     sqlTranslator,
		knobs:             knobs,
	}
}

// Start creates a background task that starts the auto span config
// reconciliation job. It also periodically ensures that the job exists,
// recreating it if it doesn't.
func (m *Manager) Start(ctx context.Context) error {
	return m.stopper.RunAsyncTask(ctx, "span-config-mgr", func(ctx context.Context) {
		m.run(ctx)
	})
}

func (m *Manager) run(ctx context.Context) {
	jobCheckCh := make(chan struct{}, 1)
	triggerJobCheck := func() {
		select {
		case jobCheckCh <- struct{}{}:
		default:
		}
	}

	// We have a few conditions that should trigger a job check:
	// - when the setting to enable/disable the reconciliation job is toggled;
	// - when the setting controlling the reconciliation job check interval is
	//   changed;
	// - when the cluster version is changed; if we don't it's possible to have
	//   started a tenant pod with a conservative view of the cluster version,
	//   skip starting the reconciliation job, learning about the cluster
	//   version shortly, and only checking the job after an interval has
	//   passed.
	jobEnabledSetting.SetOnChange(&m.settings.SV, func(ctx context.Context) {
		triggerJobCheck()
	})
	checkReconciliationJobInterval.SetOnChange(&m.settings.SV, func(ctx context.Context) {
		triggerJobCheck()
	})
	m.settings.Version.SetOnChange(func(_ context.Context, _ clusterversion.ClusterVersion) {
		triggerJobCheck()
	})

	checkJob := func() {
		if fn := m.knobs.ManagerCheckJobInterceptor; fn != nil {
			fn()
		}

		if !jobEnabledSetting.Get(&m.settings.SV) ||
			!m.settings.Version.IsActive(ctx, clusterversion.AutoSpanConfigReconciliationJob) {
			return
		}

		started, err := m.createAndStartJobIfNoneExists(ctx)
		if err != nil {
			log.Errorf(ctx, "error starting auto span config reconciliation job: %v", err)
		}
		if started {
			log.Infof(ctx, "started auto span config reconciliation job")
		}
	}

	// Periodically check if the span config reconciliation job exists and start
	// it if it doesn't.
	timer := timeutil.NewTimer()
	defer timer.Stop()

	triggerJobCheck()
	for {
		timer.Reset(checkReconciliationJobInterval.Get(&m.settings.SV))
		select {
		case <-jobCheckCh:
			checkJob()
		case <-timer.C:
			timer.Read = true
			checkJob()
		case <-m.stopper.ShouldQuiesce():
			return
		case <-ctx.Done():
			return
		}
	}
}

// createAndStartJobIfNoneExists creates span config reconciliation job iff it
// hasn't been created already and notifies the jobs registry to adopt it.
// Returns a boolean indicating if the job was created.
func (m *Manager) createAndStartJobIfNoneExists(ctx context.Context) (bool, error) {
	if m.knobs.ManagerDisableJobCreation {
		return false, nil
	}
	record := jobs.Record{
		JobID:         m.jr.MakeJobID(),
		Description:   "reconciling span configurations",
		Username:      security.RootUserName(),
		Details:       jobspb.AutoSpanConfigReconciliationDetails{},
		Progress:      jobspb.AutoSpanConfigReconciliationProgress{},
		NonCancelable: true,
	}

	var job *jobs.Job
	if err := m.db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		exists, err := jobs.RunningJobExists(ctx, jobspb.InvalidJobID, m.ie, txn,
			func(payload *jobspb.Payload) bool {
				return payload.Type() == jobspb.TypeAutoSpanConfigReconciliation
			},
		)
		if err != nil {
			return err
		}

		if fn := m.knobs.ManagerAfterCheckedReconciliationJobExistsInterceptor; fn != nil {
			fn(exists)
		}

		if exists {
			// Nothing to do here.
			job = nil
			return nil
		}
		job, err = m.jr.CreateJobWithTxn(ctx, record, record.JobID, txn)
		return err
	}); err != nil {
		return false, err
	}

	if job == nil {
		return false, nil
	}

	if fn := m.knobs.ManagerCreatedJobInterceptor; fn != nil {
		fn(job)
	}
	m.jr.NotifyToAdoptJobs(ctx)
	return true, nil
}
