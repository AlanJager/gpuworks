import { DeleteOutlined, PlusOutlined } from "@ant-design/icons";
import {
  Button,
  Checkbox,
  Col,
  Form,
  Input,
  InputNumber,
  Row,
  Select,
  Space,
} from "antd";
import { useTranslation } from "react-i18next";

type Props = {
  // The engine profile's /dev/shm size, which the server sends. It is only the
  // base of the launch's size: the server adds the CPU memory tier to it.
  shmDefaultBytes?: number;
  // The /dev/shm size the evaluated draft resolves to; an empty field takes it.
  // Absent before the first evaluation, and under --ipc=host (no size stated).
  shmResolvedBytes?: number;
  // The cluster's instance start timeout (settings, deployment group); an
  // empty field keeps following it.
  startTimeoutDefaultSec?: number;
};

const gib = 1 << 30;

// The deploy form's container override rows: environment, docker arguments,
// /dev/shm and extra mounts (CONTEXT.md, 容器覆盖). Quick and Expert render the
// same fields of one form, so the field names are fixed here.
const envName = "container_env_rows";
const mountsName = "container_mount_rows";
const dockerArgsName = "container_docker_args";
const envUnsetName = "container_env_unset";
const shmName = "container_shm_gib";
// The instance health budget is a deployment field of its own
// (start_timeout_sec), not a container override; it sits here because it is
// part of how the runtime is started.
const startTimeoutName = "start_timeout_sec";
const envAddID = "container-overrides-env-add";
const mountAddID = "container-overrides-mount-add";
const dockerArgsID = "container-overrides-docker-args";
const envUnsetID = "container-overrides-env-unset";
const shmID = "container-overrides-shm";
const startTimeoutID = "container-overrides-start-timeout";
// The server's bounds for start_timeout_sec (store.DeploymentStartTimeoutSec*).
const startTimeoutMin = 60;
const startTimeoutMax = 24 * 60 * 60;

export default function ContainerOverridesEditor({
  shmDefaultBytes,
  shmResolvedBytes,
  startTimeoutDefaultSec,
}: Props) {
  const { t } = useTranslation("deploy");
  const form = Form.useFormInstance();
  const shmDefaultGiB = shmDefaultBytes ? shmDefaultBytes / gib : undefined;
  // A stated size is what the draft resolves to, so it says nothing about what
  // an empty field would take.
  const shmStated = Form.useWatch(shmName, { form, preserve: true });
  const shmResolvedGiB =
    shmResolvedBytes && !shmStated
      ? +(shmResolvedBytes / gib).toFixed(2)
      : undefined;
  // What an empty field takes: the draft's resolved size once the server has
  // stated it, the engine default (without the tier) until then.
  const shmEmptyGiB = shmResolvedGiB ?? shmDefaultGiB;
  return (
    <Space direction="vertical" size={8} style={{ width: "100%" }}>
      <Form.Item
        label={t("containerOverrides.environment")}
        htmlFor={envAddID}
        extra={t("containerOverrides.environmentHelp")}
        style={{ marginBottom: 0 }}
      >
        <Form.List name={envName}>
          {(fields, { add, remove }) => (
            <Space direction="vertical" style={{ width: "100%" }} size={4}>
              {fields.map(({ key, ...field }) => (
                <Row
                  className="image-defaults-env-row"
                  key={key}
                  gutter={8}
                  align="middle"
                >
                  <Col flex="1">
                    <Form.Item
                      {...field}
                      name={[field.name, "key"]}
                      rules={[
                        {
                          pattern: /^[A-Za-z_][A-Za-z0-9_]*$/,
                          message: t("containerOverrides.invalidKey"),
                        },
                      ]}
                      noStyle
                    >
                      <Input placeholder="KEY" />
                    </Form.Item>
                  </Col>
                  <Col flex="2">
                    <Form.Item {...field} name={[field.name, "value"]} noStyle>
                      <Input placeholder="VALUE" />
                    </Form.Item>
                  </Col>
                  <Col>
                    <Button
                      aria-label={t("containerOverrides.deleteEnvironmentAria")}
                      type="text"
                      icon={<DeleteOutlined />}
                      onClick={() => remove(field.name)}
                    />
                  </Col>
                </Row>
              ))}
              <Button
                id={envAddID}
                type="dashed"
                size="small"
                icon={<PlusOutlined />}
                aria-label={t("containerOverrides.addEnvironment")}
                onClick={() => add({ key: "", value: "" })}
              >
                {t("containerOverrides.addEnvironment")}
              </Button>
            </Space>
          )}
        </Form.List>
      </Form.Item>
      <Form.Item
        label={t("containerOverrides.envUnset")}
        htmlFor={envUnsetID}
        extra={t("containerOverrides.envUnsetHelp")}
        style={{ marginBottom: 0 }}
      >
        <Form.Item name={envUnsetName} noStyle>
          <Select
            id={envUnsetID}
            mode="tags"
            tokenSeparators={[",", " ", "\n"]}
            placeholder="HF_HUB_OFFLINE"
            style={{ width: "100%" }}
          />
        </Form.Item>
      </Form.Item>
      <Form.Item
        label={t("containerOverrides.dockerArgs")}
        htmlFor={dockerArgsID}
        extra={t("containerOverrides.dockerArgsHelp")}
        style={{ marginBottom: 0 }}
      >
        <Form.Item name={dockerArgsName} noStyle>
          <Select
            id={dockerArgsID}
            mode="tags"
            tokenSeparators={["\n"]}
            placeholder={t("containerOverrides.dockerArgsPlaceholder")}
            style={{ width: "100%" }}
          />
        </Form.Item>
      </Form.Item>
      <Form.Item
        label={t("containerOverrides.shm")}
        htmlFor={shmID}
        name={shmName}
        extra={
          shmResolvedGiB
            ? t("containerOverrides.shmHelpResolved", { size: shmResolvedGiB })
            : shmDefaultGiB
              ? t("containerOverrides.shmHelpDefault", { size: shmDefaultGiB })
              : t("containerOverrides.shmHelp")
        }
        style={{ marginBottom: 0 }}
      >
        <InputNumber
          id={shmID}
          min={0}
          step={1}
          precision={2}
          suffix="GiB"
          placeholder={shmEmptyGiB ? String(shmEmptyGiB) : undefined}
          style={{ width: 200 }}
        />
      </Form.Item>
      <Form.Item
        label={t("containerOverrides.startTimeout")}
        htmlFor={startTimeoutID}
        name={startTimeoutName}
        extra={
          startTimeoutDefaultSec
            ? t("containerOverrides.startTimeoutHelpDefault", {
                seconds: startTimeoutDefaultSec,
              })
            : t("containerOverrides.startTimeoutHelp")
        }
        style={{ marginBottom: 0 }}
      >
        <InputNumber
          id={startTimeoutID}
          min={startTimeoutMin}
          max={startTimeoutMax}
          step={60}
          precision={0}
          suffix={t("containerOverrides.seconds")}
          placeholder={
            startTimeoutDefaultSec ? String(startTimeoutDefaultSec) : undefined
          }
          style={{ width: 200 }}
        />
      </Form.Item>
      <Form.Item
        label={t("containerOverrides.mounts")}
        htmlFor={mountAddID}
        extra={t("containerOverrides.mountsHelp")}
        style={{ marginBottom: 0 }}
      >
        <Form.List name={mountsName}>
          {(fields, { add, remove }, { errors }) => (
            <Space direction="vertical" style={{ width: "100%" }} size={4}>
              {fields.map(({ key, ...field }) => {
                const volumeSource =
                  Boolean(
                    form.getFieldValue([mountsName, field.name, "volume"]),
                  ) &&
                  !form.getFieldValue([mountsName, field.name, "host_path"]);
                return (
                  <Row
                    className="image-defaults-mount-row"
                    key={key}
                    gutter={8}
                    align="middle"
                  >
                    <Col flex="1">
                      <Form.Item
                        {...field}
                        name={[
                          field.name,
                          volumeSource ? "volume" : "host_path",
                        ]}
                        noStyle
                      >
                        <Input
                          placeholder={t(
                            volumeSource
                              ? "containerOverrides.volumePlaceholder"
                              : "containerOverrides.hostPathPlaceholder",
                          )}
                        />
                      </Form.Item>
                    </Col>
                    <Col flex="1">
                      <Form.Item
                        {...field}
                        name={[field.name, "container_path"]}
                        noStyle
                      >
                        <Input
                          placeholder={t(
                            "containerOverrides.containerPathPlaceholder",
                          )}
                        />
                      </Form.Item>
                    </Col>
                    <Col flex="0 0 125px">
                      <Form.Item
                        {...field}
                        name={[field.name, "purpose"]}
                        noStyle
                      >
                        <Select
                          style={{ width: "100%" }}
                          options={[
                            {
                              value: "models",
                              label: t("containerOverrides.purpose.models"),
                            },
                            { value: "kv-cache", label: "KV Cache" },
                            {
                              value: "data",
                              label: t("containerOverrides.purpose.data"),
                            },
                            {
                              value: "scratch",
                              label: t("containerOverrides.purpose.scratch"),
                            },
                          ]}
                          placeholder={t("containerOverrides.purpose.label")}
                        />
                      </Form.Item>
                    </Col>
                    <Col flex="0 0 72px">
                      <Form.Item
                        {...field}
                        name={[field.name, "read_only"]}
                        valuePropName="checked"
                        noStyle
                      >
                        <Checkbox>{t("containerOverrides.readOnly")}</Checkbox>
                      </Form.Item>
                    </Col>
                    <Col>
                      <Button
                        aria-label={t("containerOverrides.deleteMountAria")}
                        type="text"
                        icon={<DeleteOutlined />}
                        onClick={() => remove(field.name)}
                      />
                    </Col>
                  </Row>
                );
              })}
              <Button
                id={mountAddID}
                type="dashed"
                size="small"
                icon={<PlusOutlined />}
                aria-label={t("containerOverrides.addMount")}
                onClick={() =>
                  add({ host_path: "", container_path: "", purpose: "data" })
                }
              >
                {t("containerOverrides.addMount")}
              </Button>
              {/* The list itself, not a row, carries an error the server
                  attributes to the whole mounts section (e.g. a duplicate
                  container target); without this the message never renders. */}
              <Form.ErrorList errors={errors} />
            </Space>
          )}
        </Form.List>
      </Form.Item>
    </Space>
  );
}
