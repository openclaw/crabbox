declare module "ssh2" {
  export interface ClientChannel {
    stderr: {
      on(event: "data", listener: (data: Uint8Array) => void): void;
    };
    write(data: string | Uint8Array): boolean;
    close(): void;
    pause(): this;
    resume(): this;
    setWindow(rows: number, cols: number, height: number, width: number): void;
    on(event: "data", listener: (data: Uint8Array) => void): this;
    off(event: "close" | "drain", listener: () => void): this;
    once(event: "close", listener: () => void): this;
    once(event: "drain", listener: () => void): this;
  }

  export class Client {
    connect(config: {
      host: string;
      port: number;
      username: string;
      privateKey: string;
      readyTimeout: number;
      keepaliveInterval: number;
      keepaliveCountMax: number;
      hostHash: "sha256";
      hostVerifier: (fingerprint: string) => boolean;
    }): this;
    shell(
      window: {
        term: string;
        cols: number;
        rows: number;
        width: number;
        height: number;
      },
      callback: (error: Error | undefined, channel: ClientChannel) => void,
    ): this;
    end(): void;
    once(event: "ready" | "close", listener: () => void): this;
    once(event: "error", listener: (error: Error) => void): this;
  }

  export const utils: {
    generateKeyPairSync(
      keyType: "ed25519",
      options?: { comment?: string },
    ): { private: string; public: string };
  };
}
