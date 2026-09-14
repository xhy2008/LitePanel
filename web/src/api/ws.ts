// 单连接多路复用（设计 15 节）：一条 WS 承载全部频道，
// 断线指数退避重连，重连后重放订阅并带 last_seq 请求补帧。
import { decodeTermFrame, encodeTermFrame } from './wsFrame';

export type WsStatus = 'connecting' | 'online' | 'offline';

export interface WsOptions {
  url: () => string;
  baseDelay?: number;
  maxDelay?: number;
  onStatus?: (s: WsStatus) => void;
}

type Handler = (data: any, seq: number) => void;
type BinHandler = (payload: Uint8Array) => void;

interface ControlFrame {
  ch: string;
  t: string;
  seq?: number;
  d?: unknown;
}

interface Sub {
  lastSeq: number;
  handlers: Set<Handler>;
  binHandlers: Set<BinHandler>;
}

export function createWsClient(opts: WsOptions) {
  const baseDelay = opts.baseDelay ?? 1000;
  const maxDelay = opts.maxDelay ?? 30_000;
  const subs = new Map<string, Sub>();

  let sock: WebSocket | null = null;
  let attempt = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;
  let closedByUs = false;
  let status: WsStatus = 'offline';

  function setStatus(s: WsStatus) {
    if (status === s) return;
    status = s;
    opts.onStatus?.(s);
  }

  function subOf(ch: string): Sub {
    let s = subs.get(ch);
    if (!s) {
      s = { lastSeq: 0, handlers: new Set(), binHandlers: new Set() };
      subs.set(ch, s);
    }
    return s;
  }

  function isOpen(): boolean {
    return !!sock && sock.readyState === WebSocket.OPEN;
  }

  function sendControl(f: ControlFrame) {
    if (!isOpen()) return;
    sock!.send(JSON.stringify(f));
  }

  function flushSubs() {
    for (const [ch, s] of subs) {
      // 带上 last_seq，让服务端补发断线期间漏掉的帧（设计 15 节）。
      sendControl({ ch, t: 'sub', last_seq: s.lastSeq || undefined } as ControlFrame);
    }
  }

  function scheduleReconnect() {
    if (closedByUs || timer) return;
    const delay = Math.min(maxDelay, baseDelay * 2 ** attempt);
    attempt += 1;
    timer = setTimeout(() => {
      timer = null;
      connect();
    }, delay);
  }

  function connect() {
    setStatus('connecting');
    const ws = new WebSocket(opts.url());
    ws.binaryType = 'arraybuffer';
    sock = ws;

    ws.addEventListener('open', () => {
      attempt = 0;
      setStatus('online');
      flushSubs();
    });

    ws.addEventListener('close', () => {
      if (sock === ws) sock = null;
      setStatus('offline');
      scheduleReconnect();
    });

    ws.addEventListener('message', (ev: MessageEvent) => {
      if (typeof ev.data === 'string') {
        onText(ev.data);
        return;
      }
      onBinary(new Uint8Array(ev.data as ArrayBuffer));
    });
  }

  function onText(raw: string) {
    let f: ControlFrame;
    try {
      f = JSON.parse(raw) as ControlFrame;
    } catch {
      return; // 坏帧直接忽略，不打断连接
    }
    if (f.t !== 'data') return; // err 帧等由调用方按需处理
    const s = subs.get(f.ch);
    if (!s) return;
    if (typeof f.seq === 'number') s.lastSeq = f.seq;
    for (const h of [...s.handlers]) h(f.d, f.seq ?? 0);
  }

  function onBinary(raw: Uint8Array) {
    const decoded = decodeTermFrame(raw);
    if (!decoded) return;
    const s = subs.get(decoded.channel);
    if (!s) return;
    for (const h of [...s.binHandlers]) h(decoded.payload);
  }

  function subscribe(ch: string, h: Handler) {
    const s = subOf(ch);
    const first = s.handlers.size === 0 && s.binHandlers.size === 0;
    s.handlers.add(h);
    if (first) sendControl({ ch, t: 'sub' });
    return () => unsubscribe(ch, h);
  }

  function subscribeTerm(ch: string, h: BinHandler) {
    const s = subOf(ch);
    const first = s.handlers.size === 0 && s.binHandlers.size === 0;
    s.binHandlers.add(h);
    if (first) sendControl({ ch, t: 'sub' });
    return () => unsubscribeTerm(ch, h);
  }

  // 只有该频道最后一个订阅者退出时才真的发 unsub，
  // 否则会让后端订阅计数抖动，误停 metrics 采集（D6）。
  function unsubscribe(ch: string, h: Handler) {
    const s = subs.get(ch);
    if (!s) return;
    s.handlers.delete(h);
    if (s.handlers.size === 0 && s.binHandlers.size === 0) {
      subs.delete(ch);
      sendControl({ ch, t: 'unsub' });
    }
  }

  function unsubscribeTerm(ch: string, h: BinHandler) {
    const s = subs.get(ch);
    if (!s) return;
    s.binHandlers.delete(h);
    if (s.handlers.size === 0 && s.binHandlers.size === 0) {
      subs.delete(ch);
      sendControl({ ch, t: 'unsub' });
    }
  }

  function sendTerm(ch: string, payload: Uint8Array) {
    if (!isOpen()) {
      // 终端键入静默丢失比报错更难排查，必须显式失败。
      throw new Error('ws: 未连接，无法发送终端输入');
    }
    sock!.send(encodeTermFrame(ch, payload));
  }

  function close() {
    closedByUs = true;
    if (timer) {
      clearTimeout(timer);
      timer = null;
    }
    sock?.close();
    sock = null;
  }

  connect();

  return {
    subscribe,
    subscribeTerm,
    unsubscribe,
    unsubscribeTerm,
    sendTerm,
    sendControl,
    close,
    get status() {
      return status;
    },
    isOpen,
  };
}

export type WsClient = ReturnType<typeof createWsClient>;

let singleton: WsClient | null = null;

/** 浏览器默认实例：与页面同源，路径 /ws。 */
export function ws(): WsClient {
  if (!singleton) {
    singleton = createWsClient({
      url: () => `${location.protocol === 'https:' ? 'wss:' : 'ws:'}//${location.host}/ws`,
    });
  }
  return singleton;
}

export function closeWs() {
  singleton?.close();
  singleton = null;
}
