import { fromNodeProviderChain } from "@aws-sdk/credential-providers";

export interface NodeAWSCredentials {
  accessKeyId: string;
  secretAccessKey: string;
  sessionToken?: string;
}

export type NodeAWSCredentialProvider = () => Promise<NodeAWSCredentials>;

type NodeAWSCredentialSource = ReturnType<typeof fromNodeProviderChain>;

export function nodeAWSCredentialProvider(
  source: NodeAWSCredentialSource = fromNodeProviderChain(),
): NodeAWSCredentialProvider {
  return async () => {
    const credentials = await source();
    const session = credentials.sessionToken;
    return {
      accessKeyId: credentials.accessKeyId,
      secretAccessKey: credentials.secretAccessKey,
      ...(session ? { sessionToken: session } : {}),
    };
  };
}
