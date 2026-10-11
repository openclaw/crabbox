import type { CloudProvider } from "./fleet";
import { leaseProviderCleanupConfirmed } from "./lease-cleanup";

// Inventory and SSH access use normal lease records. Agent polling, rather than
// cloud API dispatch, owns allocation and deletion of served containers.
export function serveCloudProvider(): CloudProvider {
  const unsupported = async (): Promise<never> => {
    throw new Error("serve operation requires the outbound agent lifecycle");
  };
  return {
    listCrabboxServers: async () => [],
    supportsSSHHostKeyInjection: () => false,
    createServerWithFallback: unsupported,
    releaseLease: async (lease) => {
      if (!leaseProviderCleanupConfirmed(lease))
        throw new Error("serve container cleanup awaits agent confirmation");
    },
    deleteServer: unsupported,
    supportsNativeImages: () => false,
    nativeImagesUnsupportedMessage: () => "serve does not support native images",
    defaultImageStrategy: () => "image",
    validateLeaseImageStrategy: () => "serve does not support native images",
    createLeaseImage: unsupported,
    getImage: unsupported,
    deleteImage: unsupported,
    storedImageMetadata: async () => undefined,
    decorateImage: (image) => image,
    validateDeleteImage: async () => ({ status: 400, body: { error: "serve_images_unsupported" } }),
    deleteSSHKey: unsupported,
    hourlyPriceUSD: async () => 0,
  };
}
