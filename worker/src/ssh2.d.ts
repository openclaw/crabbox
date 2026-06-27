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
    once(event: "exit", listener: (code?: number) => void): this;
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
      algorithms: {
        serverHostKey: ["ssh-ed25519"];
        cipher: ["aes128-ctr", "aes192-ctr", "aes256-ctr"];
        hmac: [
          "hmac-sha2-256-etm@openssh.com",
          "hmac-sha2-512-etm@openssh.com",
          "hmac-sha2-256",
          "hmac-sha2-512",
        ];
      };
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
    exec(
      command: string,
      callback: (error: Error | undefined, channel: ClientChannel) => void,
    ): this;
    forwardOut(
      sourceIP: string,
      sourcePort: number,
      destinationIP: string,
      destinationPort: number,
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

  const ssh2: {
    Client: typeof Client;
    utils: typeof utils;
  };
  export default ssh2;
}
