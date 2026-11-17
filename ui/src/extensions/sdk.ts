// The one import surface for console overlay modules (ui/src/extensions/<id>/).
// An overlay imports from this file (and from its own directory) only; the
// ee_ui_internal_imports architecture budget counts every other import. Each
// name here is a stable contract: removing or renaming one breaks overlays,
// so tests/consoleExtensionsSdk.test.ts pins the list and a change to it is a
// deliberate one. Add names rather than changing them.

// SDK_MAJOR is the major version of this surface. It changes only when a name
// is removed or changes meaning; an overlay records the major it was built
// against.
export const SDK_MAJOR = 1;

// React, so an overlay builds against the console's single copy.
export { createContext, createElement, Fragment, useCallback, useContext, useEffect, useId, useMemo, useReducer, useRef, useState } from "react";
export type { ComponentType, ReactNode } from "react";

// Translation and routing.
export { Trans, useTranslation } from "react-i18next";
export { Link, Navigate, useLocation, useNavigate, useParams, useSearchParams } from "react-router-dom";

// The API client: reads and writes under /api/v1 (an extension's own routes are under /api/v1/x/<name>/) with the console's session and error decoding.
export { getJSON, sendJSON } from "../api";
export type { List } from "../api";
// A file answer saved by the browser, a Blob saved as a file, and a multipart upload.
export { downloadFile, saveBlob, sendForm } from "../apiFiles";
export type { DownloadOptions } from "../apiFiles";
export { APIError, apiErrorText, productMessageText } from "../apiError";
export type { ProductMessage } from "../apiError";

// Console building blocks (ui-kit and the Zeron components it is made of).
export { default as PageHeader } from "../components/PageHeader";
export { default as EmptyState } from "../components/EmptyState";
export { ConsoleIcon } from "../ui-kit/consoleIcons";
export { Notice } from "../ui-kit/Notice";
export { Result } from "../ui-kit/Result";
export { Spinner } from "../ui-kit/Spinner";
export { HelpTip } from "../ui-kit/HelpTip";
export { ConfirmPopover } from "../ui-kit/ConfirmPopover";
export { SideDrawer } from "../ui-kit/SideDrawer";
export { Timeline } from "../ui-kit/Timeline";
export { Progress } from "../ui-kit/Progress";
export { confirm, message } from "../ui-kit/feedback";
export { Button } from "../components/ui/button";
export { Badge } from "../components/ui/badge";
export { Input } from "../components/ui/input";
export { Switch } from "../components/ui/switch";
export { Tooltip } from "../components/ui/tooltip";
export { TabItem, TabPanel, Tabs, TabsList } from "../components/ui/tabs";
export { Dialog, DialogClose, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from "../components/ui/dialog";
export { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../components/ui/table";
export { Field, FieldDescription, FieldError, FieldLabel } from "../components/ui/field";
export { Container, ContainerBody, ContainerHeader } from "../components/ui/container";
export { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from "../components/ui/accordion";
export { MetricCard } from "../components/ui/metric-card";
export { FileUpload } from "../ui-kit/FileUpload";
// Copy to the clipboard, falling back to a dialog with the text selected when the browser refuses.
export { default as CopyButton, useCopy } from "../components/CopyButton";
export type { CopyOptions } from "../components/CopyButton";

// The overlay contract itself.
export type {
  ConsoleExtension, ExtensionHeaderItem, ExtensionLocales, ExtensionNavGroup, ExtensionNavItem, ExtensionRoute,
  ExtensionSettingsTab, LockedSlotProps, SlotProps,
} from "./types";
export type { FeatureAccess } from "../consoleManifest";
export { LockedFeature } from "./LockedFeature";
