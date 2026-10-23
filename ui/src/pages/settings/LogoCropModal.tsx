import { useEffect, useMemo, useState, type CSSProperties } from "react";
import { Button, Checkbox, Modal, Space, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import ReactCrop, { type PercentCrop } from "react-image-crop";
import { FULL_CROP, LogoError, prepareLogo, squareCrop, type LogoCrop } from "../../brandingLogo";
import { LOGO_BOX } from "../../branding";

const { Text } = Typography;


const toPercent = (crop: LogoCrop): PercentCrop => ({
  unit: "%", x: crop.x * 100, y: crop.y * 100, width: crop.width * 100, height: crop.height * 100,
});
const toFraction = (crop: PercentCrop): LogoCrop => ({
  x: crop.x / 100, y: crop.y / 100, width: crop.width / 100, height: crop.height / 100,
});

export interface LogoCropModalProps {
  /** The picked image; the dialog is open while it is set. */
  file: File | null;
  onCancel: () => void;
  /** Receives the cropped, compressed logo as a data: URL. */
  onConfirm: (logo: string) => void;
  /** prepare turns the crop into a logo; tests replace the browser codec. */
  prepare?: (file: File, crop: LogoCrop) => Promise<string>;
}

/**
 * LogoCropModal lets the administrator keep part of an uploaded logo. The
 * selection starts as the whole image; "square" snaps it to the largest
 * centered square and keeps it square while it is moved or resized.
 */
export default function LogoCropModal({ file, onCancel, onConfirm, prepare = prepareLogo }: LogoCropModalProps) {
  const { t } = useTranslation("settings");
  const [crop, setCrop] = useState<PercentCrop>(toPercent(FULL_CROP));
  const [square, setSquare] = useState(false);
  const [size, setSize] = useState<{ width: number; height: number }>();
  const [busy, setBusy] = useState(false);
  const url = useMemo(() => (file ? URL.createObjectURL(file) : ""), [file]);
  useEffect(() => () => { if (url) URL.revokeObjectURL(url); }, [url]);
  // A new image starts over from the whole picture.
  useEffect(() => {
    setCrop(toPercent(FULL_CROP));
    setSquare(false);
    setSize(undefined);
  }, [file]);

  const toggleSquare = (on: boolean) => {
    setSquare(on);
    setCrop(toPercent(on && size ? squareCrop(size.width, size.height) : FULL_CROP));
  };

  const confirm = async () => {
    if (!file) return;
    setBusy(true);
    try {
      onConfirm(await prepare(file, toFraction(crop)));
    } catch (error) {
      const reason = error instanceof LogoError ? error.reason : "unreadable";
      message.error(t(`branding.logoError.${reason}`, { max: 128 }));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      title={t("branding.crop.title")}
      open={file !== null}
      onCancel={onCancel}
      // A drag that ends past the dialog's edge releases over the mask; that
      // must not throw the crop away.
      maskClosable={false}
      destroyOnHidden
      footer={(
        <Space>
          <Button onClick={() => toggleSquare(square)} disabled={busy}>{t("branding.crop.reset")}</Button>
          <Button onClick={onCancel} disabled={busy}>{t("common.cancel")}</Button>
          <Button type="primary" loading={busy} disabled={!size} onClick={() => void confirm()}>{t("branding.crop.confirm")}</Button>
        </Space>
      )}
    >
      <Space direction="vertical" size={12} style={{ width: "100%" }}>
        <Text type="secondary">{t("branding.crop.help")}</Text>
        <div className="logo-crop-stage">
          {url && (
            <ReactCrop
              crop={crop}
              onChange={(_, percent) => setCrop(percent)}
              aspect={square ? 1 : undefined}
              keepSelection
              minWidth={8}
              minHeight={8}
              ariaLabels={{
                cropArea: t("branding.crop.area"),
                nwDragHandle: t("branding.crop.handle"),
                nDragHandle: t("branding.crop.handle"),
                neDragHandle: t("branding.crop.handle"),
                eDragHandle: t("branding.crop.handle"),
                seDragHandle: t("branding.crop.handle"),
                sDragHandle: t("branding.crop.handle"),
                swDragHandle: t("branding.crop.handle"),
                wDragHandle: t("branding.crop.handle"),
              }}
            >
              <img
                src={url}
                alt={t("branding.crop.image")}
                className="logo-crop-image"
                onLoad={(event) => {
                  const image = event.currentTarget;
                  setSize({ width: image.naturalWidth || 1, height: image.naturalHeight || 1 });
                }}
              />
            </ReactCrop>
          )}
        </div>
        <Space size={16} wrap align="center">
          <Checkbox checked={square} disabled={!size} onChange={(event) => toggleSquare(event.target.checked)}>
            {t("branding.crop.square")}
          </Checkbox>
          <Space size={8} align="center">
            <Text type="secondary">{t("branding.crop.preview")}</Text>
            <span className="logo-crop-preview" data-testid="logo-crop-preview">
              {url && size && <span style={previewStyle(url, crop, size)} />}
            </span>
          </Space>
        </Space>
      </Space>
    </Modal>
  );
}

/**
 * previewStyle shows the selection the way the top bar will: keeping its
 * proportions, grown until either side reaches LOGO_BOX.
 */
function previewStyle(url: string, crop: PercentCrop, size: { width: number; height: number }): CSSProperties {
  const w = Math.max(1, (crop.width / 100) * size.width);
  const h = Math.max(1, (crop.height / 100) * size.height);
  const scale = Math.min(LOGO_BOX.width / w, LOGO_BOX.height / h);
  const position = (offset: number, extent: number) => (extent >= 100 ? 0 : (offset / (100 - extent)) * 100);
  return {
    display: "block",
    width: Math.round(w * scale),
    height: Math.round(h * scale),
    backgroundImage: `url("${url}")`,
    backgroundRepeat: "no-repeat",
    backgroundSize: `${10000 / crop.width}% ${10000 / crop.height}%`,
    backgroundPosition: `${position(crop.x, crop.width)}% ${position(crop.y, crop.height)}%`,
  };
}
