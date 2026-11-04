import {
  ApartmentOutlined, ApiOutlined, AppstoreOutlined, BarChartOutlined, CloudOutlined,
  CloudServerOutlined, ContainerOutlined, ControlOutlined, DashboardOutlined, DatabaseOutlined,
  ExperimentOutlined, FileTextOutlined, FundOutlined, HeartOutlined, InboxOutlined, KeyOutlined,
  ProfileOutlined, RocketOutlined, ScheduleOutlined, SettingOutlined,
} from "@ant-design/icons";
import type { ReactNode } from "react";

export interface NavItem {
  key: string;
  icon: ReactNode;
  labelKey: string;
  /** Other first path segments filed under this entry, e.g. the create page of a list. */
  also?: string[];
  /** The entry is named after its group, so its own page shows no section crumb. */
  namesGroup?: boolean;
}

export interface NavGroup {
  /** Absent for the entries above the first group (the overview). */
  labelKey?: string;
  items: NavItem[];
}

// The side menu, also read by PageHeader so a page's breadcrumb names the
// same section the menu files it under. Monitoring and analysis come first,
// then daily work (serving, models), then the platform (compute,
// administration). New features go to the end of their group so existing
// entries keep their place; monitoring and analysis holds at most five
// entries (alerts would be the fifth), and further views become tabs.
export const navGroups: NavGroup[] = [
  { items: [
    { key: "/dashboard", icon: <DashboardOutlined />, labelKey: "navigation.dashboard" },
  ] },
  { labelKey: "navigation.groups.observability", items: [
    { key: "/monitor", icon: <HeartOutlined />, labelKey: "navigation.monitor" },
    { key: "/scheduling", icon: <ScheduleOutlined />, labelKey: "navigation.scheduling" },
    { key: "/analytics", icon: <BarChartOutlined />, labelKey: "navigation.analytics" },
    { key: "/logs", icon: <FileTextOutlined />, labelKey: "navigation.logs" },
  ] },
  { labelKey: "navigation.groups.inference", items: [
    // Deploying is the list's create action, so /deploy highlights the list.
    { key: "/deployments", icon: <RocketOutlined />, labelKey: "navigation.deployments", also: ["deploy"], namesGroup: true },
    { key: "/batches", icon: <InboxOutlined />, labelKey: "navigation.batches" },
    { key: "/playground/chat", icon: <ExperimentOutlined />, labelKey: "navigation.playground" },
    { key: "/evaluations", icon: <FundOutlined />, labelKey: "navigation.evaluations" },
  ] },
  { labelKey: "navigation.groups.models", items: [
    { key: "/models", icon: <AppstoreOutlined />, labelKey: "navigation.models" },
    { key: "/inference-templates", icon: <ProfileOutlined />, labelKey: "navigation.inferenceTemplates" },
    { key: "/images", icon: <ContainerOutlined />, labelKey: "navigation.images" },
  ] },
  { labelKey: "navigation.groups.compute", items: [
    { key: "/nodes", icon: <CloudServerOutlined />, labelKey: "navigation.nodes" },
    { key: "/gpus", icon: <ControlOutlined />, labelKey: "navigation.gpus" },
    { key: "/storage", icon: <DatabaseOutlined />, labelKey: "navigation.storage" },
    { key: "/networks", icon: <ApiOutlined />, labelKey: "navigation.networks" },
    { key: "/providers", icon: <CloudOutlined />, labelKey: "navigation.publicCloud" },
    { key: "/zstack", icon: <ApartmentOutlined />, labelKey: "navigation.privateCloud" },
  ] },
  { labelKey: "navigation.groups.admin", items: [
    { key: "/apikeys", icon: <KeyOutlined />, labelKey: "navigation.apiKeys" },
    { key: "/settings", icon: <SettingOutlined />, labelKey: "navigation.settings" },
  ] },
];

function pathSection(pathname: string) {
  return pathname.split("/")[1] || "";
}

/** navItemForPath returns the menu entry a path belongs to, by its first segment. */
export function navItemForPath(pathname: string) {
  const section = pathSection(pathname);
  for (const group of navGroups) {
    const item = group.items.find((entry) => entry.key.split("/")[1] === section || entry.also?.includes(section));
    if (item) return { group, item };
  }
  return undefined;
}

/**
 * sectionLabelKey is the breadcrumb's section: the entry's group. It is absent
 * for the overview, which sits above the groups, and under an entry named
 * after its group: the list's own page would repeat its title, and its other
 * pages (the deploy page) link back to the list by that same name instead.
 */
export function sectionLabelKey(pathname: string) {
  const placed = navItemForPath(pathname);
  if (!placed || placed.item.namesGroup) return undefined;
  return placed.group.labelKey;
}

/**
 * selectedNavKey is the menu item key for a path. A nested entry such as
 * /playground/chat still matches /playground/audio, so the item highlights.
 */
export function selectedNavKey(pathname: string) {
  return navItemForPath(pathname)?.item.key ?? `/${pathSection(pathname) || "dashboard"}`;
}
