import type { CoordinatorStorageView } from "./coordinator-runtime";

// Read ordered, bounded pages lazily. Callers own transactions, record policy,
// and early termination; a consumer that stops never fetches another page.
export async function* coordinatorStorageEntries<T>(
  storage: Pick<CoordinatorStorageView, "list">,
  options: { prefix: string; limit: number; noCache?: boolean },
): AsyncGenerator<[string, T]> {
  let startAfter: string | undefined;
  for (;;) {
    // oxlint-disable-next-line eslint/no-await-in-loop -- each page follows the previous page's final key.
    const page = await storage.list<T>({
      ...options,
      ...(startAfter ? { startAfter } : {}),
    });
    if (page.size === 0) return;
    yield* page;
    const next = [...page.keys()].at(-1);
    if (!next || next === startAfter) {
      throw new Error(`${options.prefix} record scan did not advance`);
    }
    startAfter = next;
    if (page.size < options.limit) return;
  }
}
