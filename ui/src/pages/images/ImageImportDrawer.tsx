import {
  Alert,
  Button,
  Col,
  Drawer,
  Form,
  Input,
  Row,
  Select,
  Switch,
} from "antd";
import { useEffect, useState } from "react";
import type { FormInstance } from "antd/es/form";
import {
  api,
  type ImageArchivePreview,
  type ImageCatalogEntry,
  type ImageEngineOption,
} from "../../api";
import { useForm } from "react-hook-form";
import { FormField } from "../../ui-kit/form/FormField";
import { HelpTip } from "../../ui-kit/HelpTip";
import { Input as TextInput } from "../../components/ui/input";
import { OptionSelect } from "../../ui-kit/OptionSelect";
import NodePicker from "../../nodes/NodePicker";
import ImageArchiveURLImport from "./ImageArchiveURLImport";
import ImageCommunityBrowser from "./ImageCommunityBrowser";
import { OutboundHostAccess, useOutboundHostAccess } from "../../components/OutboundHostAccess";
import { isArchiveURL } from "./ImagePageHelpers";
import { useTranslation } from "react-i18next";

export type NodeArchive = { node: string; path: string };

type Props = {
  open: boolean;
  onClose: () => void;
  form: FormInstance;
  importing: boolean;
  importCustom: (archive?: NodeArchive) => Promise<void> | void;
  hasExplicitTag: (ref: string) => boolean;
  sourceTab: "community" | "custom";
  /** Catalog versions searched for a reference matching a pasted archive. */
  versions: ImageCatalogEntry[];
  /** Opens the matched version and records the archive URL on it. */
  onArchiveURLMatched: (version: ImageCatalogEntry, preview: ImageArchivePreview) => void;
};

/**
 * ImageImportDrawer adds one community or custom image. Any reference is
 * accepted and any engine of the server's image engine dictionary can be
 * declared, callable included; the system catalog is never edited here.
 */
export default function ImageImportDrawer({
  open,
  onClose,
  form,
  importing,
  importCustom,
  hasExplicitTag,
  sourceTab,
  versions,
  onArchiveURLMatched,
}: Props) {
  const { t } = useTranslation("images");
  // The engines an image may declare, their labels and their order are the
  // server's dictionary (GET /images/engines); the form adds nothing to it.
  const [engines, setEngines] = useState<ImageEngineOption[] | null>(null);
  const [enginesError, setEnginesError] = useState<string | null>(null);
  const [authorizing, setAuthorizing] = useState(false);
  const [importMethod, setImportMethod] = useState("url");
  const loadNodeFile = sourceTab === "custom" && importMethod === "local";
  const archive = useForm<NodeArchive>({ defaultValues: { node: "", path: "" } });
  useEffect(() => {
    if (!open) {
      setImportMethod("url");
      archive.reset();
    }
  }, [open, archive]);
  const sources = Form.useWatch("registry_sources", { form, preserve: true }) as string[] | undefined;
  const access = useOutboundHostAccess((sources ?? []).filter(isArchiveURL), open);
  const save = async () => {
    if (authorizing || importing) return;
    try {
      if (loadNodeFile && !(await archive.trigger())) return;
      await form.validateFields();
      setAuthorizing(true);
      const file = loadNodeFile ? archive.getValues() : undefined;
      await access.authorize();
      await importCustom(file);
    } catch { /* The form and permission control retain their own errors. */ }
    finally { setAuthorizing(false); }
  };
  useEffect(() => {
    if (!open || engines) return;
    let alive = true;
    setEnginesError(null);
    api
      .imageEngines()
      .then((options) => alive && setEngines(options.items))
      .catch((error: unknown) => {
        if (alive) setEnginesError(error instanceof Error ? error.message : String(error));
      });
    return () => {
      alive = false;
    };
  }, [open, engines]);
  const engineOptions = (engines ?? []).map((option) => ({ value: option.name, label: option.label }));
  return (
    <Drawer title={t(`import.title.${sourceTab}`)} open={open} onClose={authorizing || importing ? undefined : onClose} width={640}>
      <div style={{ display: "flex", flexDirection: "column", gap: 20, width: "100%" }}>
        {sourceTab === "community" && (
          <ImageCommunityBrowser
            onPick={(pick) => {
              form.setFieldsValue({
                image_ref: pick.imageRef,
                engine: pick.engine || undefined,
                framework: pick.framework || undefined,
                engine_version: pick.engineVersion,
                framework_version: pick.frameworkVersion,
                arch: pick.arch || "multi",
                rolling_tag: pick.rollingTag,
              });
            }}
          />
        )}
        {sourceTab === "custom" && (
          <div className="grid gap-2">
            <label htmlFor="image-import-method">{t("import.method")}</label>
            <OptionSelect
              id="image-import-method"
              aria-label={t("import.method")}
              value={importMethod}
              onChange={setImportMethod}
              disabled={importing || authorizing}
              options={[
                { value: "url", label: t("import.url.title") },
                { value: "local", label: t("import.local.enable") },
              ]}
            />
          </div>
        )}
        {loadNodeFile ? (
          <div className="grid gap-3">
            <FormField control={archive.control} name="node" label={t("nodeTable.column.node")} required
              rules={{ required: t("import.local.nodeRequired") }}>
              {field => <NodePicker {...field} aria-label={t("nodeTable.column.node")} filter={{ state: ["online"] }} />}
            </FormField>
            <FormField control={archive.control} name="path" required
              label={t("nodeTable.localDialog.path")} description={<HelpTip content={t("nodeTable.localDialog.help")} />}
              rules={{ validate: path => path.trim().startsWith("/") || t("import.local.pathRequired") }}>
              {field => <TextInput {...field} aria-label={t("nodeTable.localDialog.path")} placeholder="/data/images/runner.tar" />}
            </FormField>
          </div>
        ) : open && (
          <ImageArchiveURLImport
            showTitle={sourceTab === "community"}
            versions={versions}
            onMatched={onArchiveURLMatched}
            onPrefill={(preview) =>
              form.setFieldsValue({
                image_ref: preview.repo_tags[0],
                registry_sources: [preview.url],
              })
            }
          />
        )}
        <Alert
          type="info"
          showIcon
          message={t("import.customNotice")}
          description={t("import.customDescription")}
        />
        <Form form={form} layout="vertical" initialValues={{ arch: "multi", rolling_tag: false }}>
          <Form.Item label={t("import.seriesName")} name="series_name">
            <Input placeholder={t("import.seriesNamePlaceholder")} />
          </Form.Item>
          <Form.Item
            label={t("import.imageRef")}
            name="image_ref"
            rules={[
              { required: true, message: t("import.imageRefRequired") },
              {
                validator: (_, value?: string) =>
                  !value || hasExplicitTag(value.trim())
                    ? Promise.resolve()
                    : Promise.reject(new Error(t("import.tagRequired"))),
              },
            ]}
          >
            <Input placeholder="registry.example/team/image:v1" />
          </Form.Item>
          <Row gutter={12}>
            <Col xs={24} sm={12}>
              <Form.Item
                label={t("import.engine")}
                name="engine"
                rules={[{ required: true, message: t("import.engineRequired") }]}
              >
                <Select
                  allowClear
                  loading={engines === null && !enginesError}
                  options={engineOptions}
                  status={enginesError ? "error" : undefined}
                />
              </Form.Item>
              {enginesError && (
                <Alert
                  type="error"
                  showIcon
                  style={{ marginTop: -16, marginBottom: 16 }}
                  message={t("import.enginesFailed")}
                  description={enginesError}
                />
              )}
            </Col>
            <Col xs={24} sm={12}>
              <Form.Item label={t("import.engineVersion")} name="engine_version">
                <Input placeholder={t("import.exampleEngineVersion")} />
              </Form.Item>
            </Col>
            <Col xs={24} sm={12}>
              <Form.Item
                label={t("import.framework")}
                name="framework"
                rules={[{ required: true, message: t("import.frameworkRequired") }]}
              >
                <Select
                  allowClear
                  options={[
                    { value: "cuda", label: "CUDA" },
                    { value: "cann", label: "CANN" },
                    { value: "rocm", label: "ROCm" },
                    { value: "hggc", label: "HGGC" },
                    { value: "dtk", label: "DTK" },
                    { value: "metal", label: "Metal" },
                    { value: "xre", label: "XRE" },
                  ]}
                />
              </Form.Item>
            </Col>
            <Col xs={24} sm={12}>
              <Form.Item
                label={t("import.frameworkVersion")}
                name="framework_version"
                rules={[{ required: true, whitespace: true, message: t("import.frameworkVersionRequired") }]}
              >
                <Input placeholder={t("import.exampleFrameworkVersion")} />
              </Form.Item>
            </Col>
            <Col xs={24} sm={12}>
              <Form.Item label={t("import.hostArch")} name="arch">
                <Select
                  options={[
                    { value: "multi", label: "multi" },
                    { value: "amd64", label: "amd64" },
                    { value: "arm64", label: "arm64" },
                  ]}
                />
              </Form.Item>
            </Col>
            <Col xs={24} sm={12}>
              <Form.Item
                label={t("import.rollingTag")}
                name="rolling_tag"
                valuePropName="checked"
                extra={t("import.rollingTagHelp")}
              >
                <Switch />
              </Form.Item>
            </Col>
          </Row>
          <Form.Item
            label={t("import.registrySources")}
            name="registry_sources"
            extra={t("import.registrySourcesHelp")}
          >
            <Select mode="tags" tokenSeparators={[",", " "]} placeholder="https://containerimages.example/runner.tar" />
          </Form.Item>
          <OutboundHostAccess access={access} />
          <Button type="primary" loading={importing || authorizing} onClick={() => void save()}>
            {t("import.addImage")}
          </Button>
        </Form>
      </div>
    </Drawer>
  );
}
