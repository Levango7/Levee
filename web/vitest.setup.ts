// Vitest setup.
//
// Node >= 25 exposes native global localStorage/sessionStorage (webstorage,
// in-memory without --localstorage-file), and vitest does not clobber native
// globals when populating the jsdom environment — so tests saw the native
// stub instead of a usable Storage. Replace both with a minimal in-memory
// Storage implementation: deterministic, per test file (vitest runs each file
// in a fresh worker/module graph), and API-compatible for everything the
// client and SSO helpers use.
class MemoryStorage implements Storage {
  #map = new Map<string, string>()

  get length(): number {
    return this.#map.size
  }

  clear(): void {
    this.#map.clear()
  }

  getItem(key: string): string | null {
    return this.#map.has(key) ? (this.#map.get(key) as string) : null
  }

  key(index: number): string | null {
    return [...this.#map.keys()][index] ?? null
  }

  removeItem(key: string): void {
    this.#map.delete(key)
  }

  setItem(key: string, value: string): void {
    this.#map.set(key, String(value))
  }
}

for (const name of ['localStorage', 'sessionStorage'] as const) {
  Object.defineProperty(globalThis, name, {
    configurable: true,
    writable: true,
    value: new MemoryStorage(),
  })
}
