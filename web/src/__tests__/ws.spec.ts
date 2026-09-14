import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createWsClient } from '../api/ws';

// 假 WS：记录发出的帧，允许测试主动喂入消息 / 模拟断开。
class FakeWS {
  static instances: FakeWS[] = [];
  static OPEN = 1;
  static CONNECTING = 0;
  static CLOSED = 3;

  readyState = FakeWS.CONNECTING;
  sent: (string | Uint8Array)[] = [];
  binaryType = '';
  private listeners: Record<string, ((ev?: any) => void)[]> = {};

  constructor(public url: string) {
    FakeWS.instances.push(this);
  }
  addEventListener(type: string, fn: (ev?: any) => void) {
    (this.listeners[type] ??= []).push(fn);
  }
  send(data: string | Uint8Array) {
    this.sent.push(data);
  }
  close() {
    if (this.readyState === FakeWS.CLOSED) return;
    this.readyState = FakeWS.CLOSED;
    this.emit('close');
  }
  fail() {
    this.readyState = FakeWS.CLOSED;
    this.emit('error');
    this.emit('close');
  }
  emit(type: string, ev?: any) {
    for (const fn of this.listeners[type] ?? []) fn(ev);
  }
  open() {
    this.readyState = FakeWS.OPEN;
    this.emit('open');
  }
  frame(obj: unknown) {
    this.emit('message', { data: JSON.stringify(obj), type: 'text' });
  }
  binary(raw: Uint8Array) {
    this.emit('message', { data: raw.buffer, type: 'binary' });
  }
  jsonSent() {
    return this.sent
      .filter((s): s is string => typeof s === 'string')
      .map((s) => JSON.parse(s));
  }
}

function install(): typeof FakeWS {
  FakeWS.instances = [];
  const Ctor: any = function (_this: unknown, url: string) {
    return new FakeWS(url);
  };
  Ctor.OPEN = FakeWS.OPEN;
  Ctor.CONNECTING = FakeWS.CONNECTING;
  Ctor.CLOSED = FakeWS.CLOSED;
  vi.stubGlobal('WebSocket', Ctor);
  return FakeWS;
}

const last = () => FakeWS.instances[FakeWS.instances.length - 1];

// 测试里用极小的退避基数，避免真的等 1s。
const OPTS = { url: () => 'ws://x/ws', baseDelay: 100, maxDelay: 1600 };

beforeEach(() => {
  install();
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('ws 客户端', () => {
  it('连接就绪后发出 sub 帧', () => {
    const c = createWsClient(OPTS);
    c.subscribe('metrics', vi.fn());
    expect(last().readyState).toBe(FakeWS.CONNECTING);
    last().open();
    expect(last().jsonSent()).toEqual([
      expect.objectContaining({ ch: 'metrics', t: 'sub' }),
    ]);
  });

  it('重连后自动重放订阅（否则刷新前再也收不到推送）', () => {
    const c = createWsClient(OPTS);
    c.subscribe('metrics', vi.fn());
    last().open();
    last().close();

    vi.advanceTimersByTime(100);
    expect(FakeWS.instances.length).toBe(2);
    last().open();
    expect(last().jsonSent()).toEqual([
      expect.objectContaining({ ch: 'metrics', t: 'sub' }),
    ]);
  });

  it('收到 data 帧按频道分发；unsub 后不再分发且发出 unsub 帧', () => {
    const c = createWsClient(OPTS);
    const metrics = vi.fn();
    const other = vi.fn();
    c.subscribe('metrics', metrics);
    c.subscribe('downloads', other);
    last().open();

    last().frame({ ch: 'metrics', t: 'data', seq: 7, d: { cpu: 12 } });
    expect(metrics).toHaveBeenCalledWith({ cpu: 12 }, 7);
    expect(other).not.toHaveBeenCalled();

    c.unsubscribe('metrics', metrics);
    expect(last().jsonSent()).toContainEqual(
      expect.objectContaining({ ch: 'metrics', t: 'unsub' }),
    );
    last().frame({ ch: 'metrics', t: 'data', seq: 8, d: { cpu: 13 } });
    expect(metrics).toHaveBeenCalledTimes(1);
  });

  it('二进制终端帧按频道名分发原始字节', () => {
    const c = createWsClient(OPTS);
    const term = vi.fn();
    c.subscribeTerm('term:42', term);
    last().open();

    const ch = 'term:42';
    const payload = new Uint8Array([0x1b, 0x5b, 0x6d, 0x00, 0xff]);
    const raw = new Uint8Array(1 + ch.length + payload.length);
    raw[0] = ch.length;
    raw.set(new TextEncoder().encode(ch), 1);
    raw.set(payload, 1 + ch.length);
    last().binary(raw);

    expect(term).toHaveBeenCalledTimes(1);
    expect(Array.from(term.mock.calls[0][0] as Uint8Array)).toEqual(Array.from(payload));
  });

  it('重连订阅带上 last_seq 以补帧', () => {
    const c = createWsClient(OPTS);
    c.subscribe('metrics', vi.fn());
    last().open();
    last().frame({ ch: 'metrics', t: 'data', seq: 41, d: {} });

    last().close();
    vi.advanceTimersByTime(100);
    last().open();
    expect(last().jsonSent()[0]).toEqual(
      expect.objectContaining({ t: 'sub', ch: 'metrics', last_seq: 41 }),
    );
  });

  it('重连间隔指数退避并封顶在 maxDelay', () => {
    createWsClient(OPTS);
    const attemptsAt = (ms: number) => {
      last().close();
      vi.advanceTimersByTime(ms);
      return FakeWS.instances.length;
    };
    // 第 1 次：100ms
    expect(attemptsAt(99)).toBe(1);
    expect(attemptsAt(1)).toBe(2);
    // 第 2 次：200ms
    expect(attemptsAt(199)).toBe(2);
    expect(attemptsAt(1)).toBe(3);
    // 第 3 次：400ms
    expect(attemptsAt(399)).toBe(3);
    expect(attemptsAt(1)).toBe(4);
    // 一路封顶到 1600ms，不会无限膨胀
    for (let i = 0; i < 8; i++) {
      last().close();
      vi.advanceTimersByTime(1600);
    }
    expect(FakeWS.instances.length).toBe(12);
  });

  it('连接成功后退避归零', () => {
    createWsClient(OPTS);
    last().close();
    vi.advanceTimersByTime(100);
    last().open(); // 成功 → 归零
    last().close();
    vi.advanceTimersByTime(100); // 仍应是 100ms 而非累计
    expect(FakeWS.instances.length).toBe(3);
  });

  it('主动 close 后不再自动重连', () => {
    const c = createWsClient(OPTS);
    c.subscribe('metrics', vi.fn());
    last().open();
    c.close();
    const n = FakeWS.instances.length;
    vi.advanceTimersByTime(120_000);
    expect(FakeWS.instances.length).toBe(n);
  });

  it('sendTerm 在未连接时抛错，而不是静默丢弃键入', () => {
    const c = createWsClient(OPTS);
    expect(() => c.sendTerm('term:1', new Uint8Array([1]))).toThrow();
  });

  it('sendTerm 在已连接时发出带频道名的二进制帧', () => {
    const c = createWsClient(OPTS);
    c.subscribeTerm('term:1', vi.fn());
    last().open();
    c.sendTerm('term:1', new Uint8Array([0x6c, 0x73]));
    const sent = last().sent[last().sent.length - 1] as Uint8Array;
    expect(sent[0]).toBe(6); // 'term:1' 是 6 字节
    expect(new TextDecoder().decode(sent.slice(1, 7))).toBe('term:1');
    expect(Array.from(sent.slice(7))).toEqual([0x6c, 0x73]);
  });

  it('连接状态回调可用于界面显示 在线/离线', () => {
    const status = vi.fn();
    const c = createWsClient({ ...OPTS, onStatus: status });
    c.subscribe('metrics', vi.fn());
    expect(status).toHaveBeenLastCalledWith('connecting');
    last().open();
    expect(status).toHaveBeenLastCalledWith('online');
    last().close();
    expect(status).toHaveBeenLastCalledWith('offline');
  });

  it('同一频道多个订阅者都能收到', () => {
    const c = createWsClient(OPTS);
    const a = vi.fn();
    const b = vi.fn();
    c.subscribe('svclog:1', a);
    c.subscribe('svclog:1', b);
    last().open();
    last().frame({ ch: 'svclog:1', t: 'data', seq: 1, d: 'x' });
    expect(a).toHaveBeenCalled();
    expect(b).toHaveBeenCalled();
    // 只发一个 sub 帧，避免无谓的后端计数抖动
    expect(last().jsonSent().filter((f: any) => f.ch === 'svclog:1' && f.t === 'sub')).toHaveLength(1);
  });

  it('unsubscribe 后若该频道无人订阅才真的退订', () => {
    const c = createWsClient(OPTS);
    const a = vi.fn();
    const b = vi.fn();
    c.subscribe('svclog:1', a);
    c.subscribe('svclog:1', b);
    last().open();
    c.unsubscribe('svclog:1', a);
    expect(last().jsonSent().some((f: any) => f.t === 'unsub')).toBe(false);
    last().frame({ ch: 'svclog:1', t: 'data', seq: 2, d: 'y' });
    expect(b).toHaveBeenCalledTimes(1);
  });
});
