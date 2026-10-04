import type { MediaType, View } from "./types.ts";

export const discoverUserKey = "reelay.discover-user";
export const discoverTypeKey = "reelay.discover-type";

export const session = {
  current: "dashboard" as View,
  selectedSeries: null as number | null,
  connectionStatus: "Connecting",
  refreshing: false,
  discoverUser: localStorage.getItem(discoverUserKey) ?? "",
  discoverType: (localStorage.getItem(discoverTypeKey) === "series" ? "series" : "movie") as MediaType,
  requestsOffset: 0,
  attentionOnly: false
};

export const requestsPageSize = 100;

const unregistered = (name: string) => () => {
  throw new Error(`session hook ${name} is not registered`);
};

// app.ts registers these at startup so views can navigate or demand authentication without importing the entry module.
export const hooks = {
  navigate: unregistered("navigate") as (view: View) => Promise<void>,
  authRequired: unregistered("authRequired") as (message: string) => void
};
