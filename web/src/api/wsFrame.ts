// 二进制数据帧：[1B 频道名长度][频道名 UTF-8][原始字节]，与后端 internal/ws/proto.go 对应。
const MAX_CHANNEL_LEN = 200;
const encoder = new TextEncoder();
const decoder = new TextDecoder();

export function encodeTermFrame(channel: string, payload: Uint8Array): Uint8Array {
  const name = encoder.encode(channel).slice(0, MAX_CHANNEL_LEN);
  const raw = new Uint8Array(1 + name.length + payload.length);
  raw[0] = name.length;
  raw.set(name, 1);
  raw.set(payload, 1 + name.length);
  return raw;
}

export function decodeTermFrame(
  raw: Uint8Array,
): { channel: string; payload: Uint8Array } | null {
  if (raw.length < 1) return null;
  const n = raw[0];
  if (raw.length < 1 + n) return null;
  return {
    channel: decoder.decode(raw.slice(1, 1 + n)),
    payload: raw.slice(1 + n),
  };
}
