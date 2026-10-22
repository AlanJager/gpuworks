import type { ReactNode } from "react";
import type { TFunction } from "i18next";

/** The configuration row the reader points at, or null. */
export interface Highlight {
  lit: string | null;
  light: (ref: string | null) => void;
}

interface DiagramProps extends Highlight {
  t: TFunction;
}

// Wraps the lines and boxes one configuration row explains; everything else
// fades while another row is highlighted.
function Part({ refKey, lit, children }: { refKey: string; lit: string | null; children: ReactNode }) {
  return <g data-ref={refKey} className={lit && lit !== refKey ? "dimmed" : undefined}>{children}</g>;
}

// A numbered marker on the diagram, matching the table row of the same number.
// While another row is highlighted it fades with that row's lines.
function Marker({ refKey, n, x, y, t, lit, light }: { refKey: string; n: number; x: number; y: number } & DiagramProps) {
  return (
    <g
      className={`net-topo-marker${lit === refKey ? " on" : lit ? " dimmed" : ""}`}
      role="button"
      tabIndex={0}
      aria-label={t("topology.diagram.marker", { n })}
      aria-pressed={lit === refKey}
      onMouseEnter={() => light(refKey)}
      onMouseLeave={() => light(null)}
      onFocus={() => light(refKey)}
      onBlur={() => light(null)}
    >
      <circle cx={x} cy={y} r={11} />
      <text x={x} y={y}>{n}</text>
    </g>
  );
}

function Arrows({ prefix, lanes }: { prefix: string; lanes: string[] }) {
  return (
    <defs>
      {lanes.map((lane) => (
        <marker key={lane} id={`${prefix}-${lane}`} viewBox="0 0 10 10" refX={9} refY={5} markerWidth={7} markerHeight={7} orient="auto-start-reverse">
          <path className={`arrow-${lane}`} d="M0 0L10 5L0 10z" />
        </marker>
      ))}
    </defs>
  );
}

function Label({ x, y, size = 12, bold, muted, mono, halo, children }: { x: number; y: number; size?: number; bold?: boolean; muted?: boolean; mono?: boolean; halo?: boolean; children: ReactNode }) {
  const cls = [bold && "title", muted && "muted", mono && "mono", halo && "halo"].filter(Boolean).join(" ") || undefined;
  return <text x={x} y={y} textAnchor="middle" fontSize={size} className={cls}>{children}</text>;
}

// One interface on a compute node: a tinted pill with its role and address.
function Nic({ x, y, w, lane, title, detail }: { x: number; y: number; w: number; lane: string; title: string; detail: string }) {
  const cx = x + w / 2;
  return (
    <>
      <rect className={`fill-${lane}`} x={x} y={y} width={w} height={40} rx={2} />
      <Label x={cx} y={y + 17}>{title}</Label>
      <Label x={cx} y={y + 33} mono>{detail}</Label>
    </>
  );
}

function Actor({ x, y, w, title, sub }: { x: number; y: number; w: number; title: string; sub: string }) {
  return (
    <>
      <rect className="actor" x={x} y={y} width={w} height={62} rx={4} />
      <Label x={x + w / 2} y={y + 28} size={13} bold>{title}</Label>
      <Label x={x + w / 2} y={y + 48} muted>{sub}</Label>
    </>
  );
}

function Listen({ x, y, w, lane, title, addr }: { x: number; y: number; w: number; lane: string; title: string; addr: string }) {
  return (
    <>
      <rect className={`fill-${lane}`} x={x} y={y} width={w} height={44} rx={2} />
      <Label x={x + w / 2} y={y + 18} muted>{title}</Label>
      <Label x={x + w / 2} y={y + 36} mono>{addr}</Label>
    </>
  );
}

/**
 * One address for everything: the server answers on a single listen address,
 * the management network carries serving (through the agent tunnel) and
 * storage sync, and only multi-node inference has a network of its own.
 */
export function SimpleDiagram(props: DiagramProps) {
  const { t, lit } = props;
  const d = (key: string, opts?: Record<string, unknown>) => t(`topology.diagram.${key}`, opts);
  const node = (name: string, y: number, ip: string) => (
    <g>
      <rect className="box" x={800} y={y} width={270} height={170} rx={4} />
      <Label x={935} y={y + 28} size={14} bold>{d("node", { name })}</Label>
      <Part refKey="s2" lit={lit}><Nic x={820} y={y + 44} w={230} lane="mgmt" title={d("mgmtNic", { nic: "eth0" })} detail={ip} /></Part>
      <Part refKey="s3" lit={lit}><Nic x={820} y={y + 106} w={230} lane="compute" title="mlx5_0..7 · IB" detail={d("computeNetwork")} /></Part>
    </g>
  );
  return (
    <svg viewBox="0 0 1120 520" role="group" aria-label={d("simpleLabel")}>
      <Arrows prefix="topo-s" lanes={["edge", "mgmt"]} />
      <Part refKey="s1" lit={lit}>
        <Actor x={30} y={90} w={170} title={d("browser")} sub={d("browserSimple")} />
        <Actor x={30} y={190} w={170} title={d("client")} sub={d("clientSimple")} />
        <path className="link edge" d="M200 120 L360 150" markerEnd="url(#topo-s-edge)" />
        <path className="link edge" d="M200 220 L360 180" markerEnd="url(#topo-s-edge)" />
      </Part>
      <rect className="server" x={360} y={90} width={260} height={190} rx={4} />
      <Label x={490} y={122} size={14} bold>{d("server")}</Label>
      <Part refKey="s1" lit={lit}>
        <Listen x={385} y={142} w={210} lane="edge" title={d("consoleShared")} addr="10.1.0.10:8800" />
      </Part>
      <Label x={490} y={214} muted>{d("tunnelRoute")}</Label>
      <Label x={490} y={232} muted>{d("tunnelVia")}</Label>
      <Label x={490} y={258} muted mono>{d("loopback")}</Label>
      {node("A", 40, "10.1.0.21")}
      {node("B", 310, "10.1.0.22")}
      <Part refKey="s2" lit={lit}>
        <path className="link mgmt" d="M820 104 C 720 104, 700 170, 620 175" markerEnd="url(#topo-s-mgmt)" />
        <path className="link mgmt" d="M820 374 C 720 374, 700 210, 620 205" markerEnd="url(#topo-s-mgmt)" />
        <path className="link mgmt" d="M800 125 C 760 200, 760 290, 800 355" />
        <Label x={742} y={252} muted halo>{d("storageSync")}</Label>
        <Label x={742} y={268} muted halo>{d("nodeToNode")}</Label>
      </Part>
      <Part refKey="s3" lit={lit}>
        <path className="link compute" d="M1060 186 C 1090 260, 1090 360, 1060 416" />
        <text x={1090} y={300} textAnchor="middle" fontSize={12} className="muted halo" transform="rotate(90 1090 300)">NCCL / RDMA</text>
      </Part>
      <Marker refKey="s1" n={1} x={280} y={126} {...props} />
      <Marker refKey="s2" n={2} x={700} y={118} {...props} />
      <Marker refKey="s3" n={3} x={1082} y={232} {...props} />
    </svg>
  );
}

/**
 * A network per kind of traffic: the console, the inference API and node
 * access each listen on their own network, the gateway reaches instances
 * directly over a serving network, and nodes sync and carry compute traffic on
 * networks of their own.
 */
export function ComplexDiagram(props: DiagramProps) {
  const { t, lit } = props;
  const d = (key: string, opts?: Record<string, unknown>) => t(`topology.diagram.${key}`, opts);
  const node = (name: string, y: number, suffix: string) => (
    <g>
      <rect className="box" x={790} y={y} width={290} height={300} rx={4} />
      <Label x={935} y={y + 28} size={14} bold>{d("node", { name })}</Label>
      <Part refKey="f4" lit={lit}><Nic x={810} y={y + 44} w={250} lane="mgmt" title={d("mgmtNic", { nic: "eth0" })} detail={`10.1.0.${suffix}`} /></Part>
      <Part refKey="f5" lit={lit}><Nic x={810} y={y + 100} w={250} lane="serve" title={d("servingNic", { nic: "eth1" })} detail={`10.2.0.${suffix}:19443`} /></Part>
      <Part refKey="f6" lit={lit}><Nic x={810} y={y + 156} w={250} lane="store" title={d("storageNic", { nic: "eth2" })} detail={`10.3.0.${suffix}`} /></Part>
      <Part refKey="f7" lit={lit}><Nic x={810} y={y + 212} w={250} lane="compute" title={d("computeNic", { nic: "mlx5_0..7" })} detail={d("ibBootstrap", { nic: "eth0" })} /></Part>
    </g>
  );
  return (
    <svg viewBox="0 0 1120 700" role="group" aria-label={d("complexLabel")}>
      <Arrows prefix="topo-f" lanes={["edge", "mgmt", "serve", "host"]} />
      <Part refKey="f1" lit={lit}>
        <Actor x={20} y={80} w={170} title={d("browser")} sub={d("browserOffice")} />
        <path className="link edge" d="M190 112 L340 128" markerEnd="url(#topo-f-edge)" />
      </Part>
      <Part refKey="f2" lit={lit}>
        <Actor x={20} y={250} w={170} title={d("client")} sub={d("clientBusiness")} />
        <path className="link edge" d="M190 282 L340 222" markerEnd="url(#topo-f-edge)" />
      </Part>
      <rect className="server" x={340} y={60} width={280} height={440} rx={4} />
      <Label x={480} y={90} size={14} bold>{d("server")}</Label>
      <Part refKey="f1" lit={lit}>
        <Listen x={360} y={106} w={240} lane="edge" title={d("consoleListen")} addr="172.16.0.10:8800" />
      </Part>
      <Part refKey="f2" lit={lit}>
        <Listen x={360} y={200} w={240} lane="edge" title={d("gatewayListen")} addr="192.168.5.10:8800" />
      </Part>
      <Part refKey="f3" lit={lit}>
        <Listen x={360} y={294} w={240} lane="mgmt" title={d("agentListen")} addr="10.1.0.10:8800" />
      </Part>
      <Part refKey="f5" lit={lit}>
        <Listen x={360} y={388} w={240} lane="serve" title={d("servingDirect")} addr={d("servingTarget")} />
      </Part>
      <Label x={480} y={476} muted mono>{d("loopbackAlways")}</Label>
      {node("A", 30, "21")}
      {node("B", 370, "22")}
      <Part refKey="f3" lit={lit}>
        <path className="link mgmt" d="M810 94 C 700 94, 690 300, 600 310" markerEnd="url(#topo-f-mgmt)" />
        <path className="link mgmt" d="M810 434 C 700 434, 690 330, 600 324" markerEnd="url(#topo-f-mgmt)" />
      </Part>
      <Part refKey="f5" lit={lit}>
        <path className="link serve" d="M600 404 C 700 404, 700 150, 810 150" markerEnd="url(#topo-f-serve)" />
        <path className="link serve" d="M600 416 C 700 416, 700 490, 810 490" markerEnd="url(#topo-f-serve)" />
      </Part>
      <Part refKey="f6" lit={lit}>
        <path className="link store" d="M1060 206 C 1086 206, 1086 546, 1060 546" />
      </Part>
      <Part refKey="f7" lit={lit}>
        <path className="link compute" d="M810 262 C 770 262, 770 602, 810 602" />
      </Part>
      <Part refKey="f8" lit={lit}>
        <path className="link host dash" d="M540 500 C 560 620, 700 650, 790 640" markerEnd="url(#topo-f-host)" />
        <text x={610} y={640} fontSize={12} className="muted halo">{d("hostAccess")}</text>
      </Part>
      {[
        [1, 265, 120], [2, 265, 252], [3, 668, 232], [4, 760, 94],
        [5, 720, 470], [6, 1080, 376], [7, 770, 352], [8, 560, 585],
      ].map(([n, x, y]) => <Marker key={n} refKey={`f${n}`} n={n} x={x} y={y} {...props} />)}
    </svg>
  );
}
