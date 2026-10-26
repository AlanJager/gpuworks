import {
  CloudServerOutlined, DashboardOutlined, AppstoreOutlined, RocketOutlined,
  UnorderedListOutlined, KeyOutlined, CloudOutlined, SettingOutlined, FileSearchOutlined,
  ApartmentOutlined, ControlOutlined, ApiOutlined,
  ExperimentOutlined, HeartOutlined, BarChartOutlined,
} from "@ant-design/icons";

// The side menu, also read by PageHeader so a page's breadcrumb names the
// same section the menu files it under.
export const navGroups = [
  { labelKey: "navigation.groups.overview", items: [
    { key: "/dashboard", icon: <DashboardOutlined />, labelKey: "navigation.dashboard" },
    { key: "/monitor", icon: <HeartOutlined />, labelKey: "navigation.monitor" },
    { key: "/analytics", icon: <BarChartOutlined />, labelKey: "navigation.analytics" },
  ] },
  { labelKey: "navigation.groups.resources", items: [
    { key: "/providers", icon: <CloudOutlined />, labelKey: "navigation.publicCloud" },
    { key: "/zstack", icon: <ApartmentOutlined />, labelKey: "navigation.privateCloud" },
    { key: "/nodes", icon: <CloudServerOutlined />, labelKey: "navigation.nodes" },
    { key: "/gpus", icon: <ControlOutlined />, labelKey: "navigation.gpus" },
    { key: "/models", icon: <AppstoreOutlined />, labelKey: "navigation.models" },
	{ key: "/inference-templates", icon: <AppstoreOutlined />, labelKey: "navigation.inferenceTemplates" },
    { key: "/images", icon: <CloudOutlined />, labelKey: "navigation.images" },
    { key: "/storage", icon: <CloudServerOutlined />, labelKey: "navigation.storage" },
    { key: "/networks", icon: <ApiOutlined />, labelKey: "navigation.networks" },
  ] },
  { labelKey: "navigation.groups.inference", items: [
    { key: "/deploy", icon: <RocketOutlined />, labelKey: "navigation.deploy" },
    { key: "/deployments", icon: <UnorderedListOutlined />, labelKey: "navigation.deployments" },
    { key: "/playground/chat", icon: <ExperimentOutlined />, labelKey: "navigation.playground" },
    { key: "/evaluations", icon: <FileSearchOutlined />, labelKey: "navigation.evaluations" },
  ] },
  { labelKey: "navigation.groups.platform", items: [
    { key: "/apikeys", icon: <KeyOutlined />, labelKey: "navigation.apiKeys" },
    { key: "/logs", icon: <FileSearchOutlined />, labelKey: "navigation.logs" },
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
    const item = group.items.find((entry) => entry.key.split("/")[1] === section);
    if (item) return { group, item };
  }
  return undefined;
}

/** navGroupForPath returns the menu section a path belongs to, by its first segment. */
export function navGroupForPath(pathname: string) {
  return navItemForPath(pathname)?.group;
}

// These entries are the group itself. A section crumb would only repeat the page.
const bareSectionKeys = new Set(["/dashboard"]);

/** sectionLabelKey is the breadcrumb's section, absent when the entry is the group. */
export function sectionLabelKey(pathname: string) {
  const placed = navItemForPath(pathname);
  if (!placed || bareSectionKeys.has(placed.item.key)) return undefined;
  return placed.group.labelKey;
}

/**
 * selectedNavKey is the menu item key for a path. A nested entry such as
 * /playground/chat still matches /playground/audio, so the item highlights.
 */
export function selectedNavKey(pathname: string) {
  return navItemForPath(pathname)?.item.key ?? `/${pathSection(pathname) || "dashboard"}`;
}
