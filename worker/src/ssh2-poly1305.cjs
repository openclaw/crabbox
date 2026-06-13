"use strict";

const heap = new Uint8Array(64);
let heapOffset = 0;

function readLittleEndian(bytes) {
  let value = 0n;
  for (let index = bytes.length - 1; index >= 0; index -= 1) {
    value = (value << 8n) | BigInt(bytes[index]);
  }
  return value;
}

function writeLittleEndian(value, output) {
  for (let index = 0; index < output.length; index += 1) {
    output[index] = Number(value & 0xffn);
    value >>= 8n;
  }
}

function authenticate(output, first, firstLength, second, secondLength, key) {
  const rBytes = Uint8Array.from(key.subarray(0, 16));
  rBytes[3] &= 15;
  rBytes[7] &= 15;
  rBytes[11] &= 15;
  rBytes[15] &= 15;
  rBytes[4] &= 252;
  rBytes[8] &= 252;
  rBytes[12] &= 252;

  const r = readLittleEndian(rBytes);
  const s = readLittleEndian(key.subarray(16, 32));
  const modulus = (1n << 130n) - 5n;
  let accumulator = 0n;
  const message = new Uint8Array(firstLength + secondLength);
  message.set(first.subarray(0, firstLength));
  message.set(second.subarray(0, secondLength), firstLength);

  for (let offset = 0; offset < message.length; offset += 16) {
    const block = message.subarray(offset, Math.min(offset + 16, message.length));
    const padded = readLittleEndian(block) + (1n << BigInt(block.length * 8));
    accumulator = ((accumulator + padded) * r) % modulus;
  }

  const tag = (accumulator + s) & ((1n << 128n) - 1n);
  writeLittleEndian(tag, heap.subarray(output, output + 16));
}

module.exports = async function createPoly1305() {
  return {
    HEAPU8: heap,
    _malloc(size) {
      const pointer = heapOffset;
      heapOffset += size;
      if (heapOffset > heap.length) {
        throw new Error("ssh2 Poly1305 heap exhausted");
      }
      return pointer;
    },
    cwrap(name) {
      if (name !== "poly1305_auth") {
        throw new Error(`unsupported ssh2 Poly1305 export: ${name}`);
      }
      return authenticate;
    },
  };
};
