import type { CoordinatorStorageView } from "./coordinator-runtime";

export const storageRecordScanBatchSize = 128;

// Read ordered, bounded pages lazily. Callers own transactions, record policy,
// and early termination; a consumer that stops never fetches another page.
export async function* coordinatorStorageEntries<T>(
  storage: Pick<CoordinatorStorageView, "list">,
  options: { prefix: string; limit: number } & NonNullable<
    Parameters<CoordinatorStorageView["list"]>[0]
  >,
): AsyncGenerator<[string, T]> {
  let cursor = options.reverse ? options.end : options.startAfter;
  for (;;) {
    // oxlint-disable-next-line eslint/no-await-in-loop -- each page follows the previous page's final key.
    const page = await storage.list<T>({
      ...options,
      // Bounds retain their meaning in either direction: reverse pages lower the exclusive end.
      ...(cursor === undefined ? {} : options.reverse ? { end: cursor } : { startAfter: cursor }),
    });
    if (page.size === 0) return;
    yield* page;
    const next = [...page.keys()].at(-1);
    if (!next || next === cursor) {
      throw new Error(`${options.prefix} record scan did not advance`);
    }
    cursor = next;
    if (page.size < options.limit) return;
  }
}
