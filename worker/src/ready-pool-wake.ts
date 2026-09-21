import { provisioningDuePrefix, type CoordinatorStorageView } from "./coordinator-runtime";

export const portablePoolWakePrefix = "portable-ready-pool-v1-wake:";

function dueKey(id: string, at: number): string {
  return `${provisioningDuePrefix}${Math.trunc(at).toString().padStart(16, "0")}:pool-access:${id}`;
}

// Shares the runtime's ordered, bounded due index; the private pointer makes
// replacing a deadline transactional without scanning every pool or grant.
export async function setPoolWake(
  storage: CoordinatorStorageView,
  id: string,
  at?: number,
): Promise<void> {
  const key = `${portablePoolWakePrefix}${id}`;
  const previous = await storage.get<{ at: number }>(key);
  if (previous) await storage.delete(dueKey(id, previous.at));
  if (at === undefined) {
    await storage.delete(key);
    return;
  }
  await storage.put(key, { at });
  await storage.put(dueKey(id, at), { operationID: id, at, kind: "pool-access" });
}
