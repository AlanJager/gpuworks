// ui/src/extensions/sdk.ts is the only import surface of console overlay
// modules (the ee_ui_internal_imports budget counts any other import). Its
// names are a contract with overlays built separately: this list changes only
// by adding names, in the same change that adds them to sdk.ts.
import assert from "node:assert/strict";
import test from "node:test";
import "./helpers/dom.ts";

const sdk = await import("../src/extensions/sdk.ts");

test("the overlay SDK exports exactly the pinned runtime names", () => {
  assert.deepEqual(Object.keys(sdk).sort(), [
    "APIError", "Accordion", "AccordionContent", "AccordionItem", "AccordionTrigger", "Badge", "Button", "ConfirmPopover", "ConsoleIcon",
    "Container", "ContainerBody", "ContainerHeader", "CopyButton",
    "Dialog", "DialogClose", "DialogContent", "DialogDescription", "DialogFooter", "DialogHeader", "DialogTitle", "DialogTrigger",
    "EmptyState", "Field", "FieldDescription", "FieldError", "FieldLabel", "FileUpload", "Fragment", "HelpTip", "Input", "Link", "LockedFeature",
    "Alert", "AlertAction", "AlertDescription", "AlertIcon", "AlertTitle",
    "Card", "CardAction", "CardContent", "CardDescription", "CardFooter", "CardHeader", "CardTitle", "Checkbox",
    "DetailList", "DetailListItem", "DetailListLabel", "DetailListSection", "DetailListSectionLabel", "DetailListSeparator", "DetailListValue",
    "Textarea",
    "MetricCard", "Navigate", "Notice", "PageHeader", "Progress", "Result", "SDK_MAJOR", "SideDrawer", "Spinner", "Switch", "TabItem", "TabPanel",
    "Table", "TableBody", "TableCell", "TableHead", "TableHeader", "TableRow", "Tabs", "TabsList", "Timeline", "Tooltip", "Trans",
    "apiErrorText", "confirm", "createContext", "createElement", "downloadFile", "getJSON", "message", "productMessageText", "saveBlob",
    "sendForm", "sendJSON", "useCallback", "useContext", "useCopy", "useEffect", "useId", "useLocation", "useMemo", "useNavigate",
    "useParams", "useReducer", "useRef", "useSearchParams", "useState", "useTranslation",
  ].sort());
});

test("the SDK major is 1: it moves only when a pinned name is removed or changes meaning", () => {
  assert.equal(sdk.SDK_MAJOR, 1);
});

test("every SDK export is defined, so an overlay never imports an undefined binding", () => {
  for (const [name, value] of Object.entries(sdk)) assert.notEqual(value, undefined, name);
});
