// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

import moment from "moment";
import { createSlice, PayloadAction } from "@reduxjs/toolkit";
import { DOMAIN_NAME } from "../utils";

type StatementsDateRangeState = {
  start: number;
  end: number;
};

type SortSetting = {
  ascending: boolean;
  columnTitle: string;
};

export type LocalStorageState = {
  "adminUi/showDiagnosticsModal": boolean;
  "showColumns/StatementsPage": string;
  "showColumns/TransactionPage": string;
  "dateRange/StatementsPage": StatementsDateRangeState;
  "sortSetting/StatementsPage": SortSetting;
  "sortSetting/TransactionsPage": SortSetting;
  "sortSetting/SessionsPage": SortSetting;
};

type Payload = {
  key: keyof LocalStorageState;
  value: any;
};

const defaultDateRange: StatementsDateRangeState = {
  start: moment
    .utc()
    .subtract(1, "hours")
    .unix(),
  end: moment.utc().unix() + 60, // Add 1 minute to account for potential lag.
};

const defaultSortSetting: SortSetting = {
  ascending: false,
  columnTitle: "executionCount",
};

const defaultSessionsSortSetting: SortSetting = {
  ascending: false,
  columnTitle: "statementAge",
};

// TODO (koorosh): initial state should be restored from preserved keys in LocalStorage
const initialState: LocalStorageState = {
  "adminUi/showDiagnosticsModal":
    Boolean(JSON.parse(localStorage.getItem("adminUi/showDiagnosticsModal"))) ||
    false,
  "showColumns/StatementsPage":
    JSON.parse(localStorage.getItem("showColumns/StatementsPage")) || null,
  "showColumns/TransactionPage":
    JSON.parse(localStorage.getItem("showColumns/TransactionPage")) || null,
  "dateRange/StatementsPage":
    JSON.parse(localStorage.getItem("dateRange/StatementsPage")) ||
    defaultDateRange,
  "sortSetting/StatementsPage":
    JSON.parse(localStorage.getItem("sortSetting/StatementsPage")) ||
    defaultSortSetting,
  "sortSetting/TransactionsPage":
    JSON.parse(localStorage.getItem("sortSetting/TransactionsPage")) ||
    defaultSortSetting,
  "sortSetting/SessionsPage":
    JSON.parse(localStorage.getItem("sortSetting/SessionsPage")) ||
    defaultSessionsSortSetting,
};

const localStorageSlice = createSlice({
  name: `${DOMAIN_NAME}/localStorage`,
  initialState,
  reducers: {
    update: (state: any, action: PayloadAction<Payload>) => {
      state[action.payload.key] = action.payload.value;
    },
  },
});

export const { actions, reducer } = localStorageSlice;
