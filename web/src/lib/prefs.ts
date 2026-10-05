// Small view settings per browser (e.g. sidebar on/off). The storage may be missing or
// blocked (private window); then the default applies and nothing breaks.

const PREFIX = "agw."

type Store = Pick<Storage, "getItem" | "setItem">

const browserStore = (): Store | undefined => (typeof localStorage === "undefined" ? undefined : localStorage)

export function readFlag(key: string, fallback: boolean, store: Store | undefined = browserStore()): boolean {
  try {
    const v = store?.getItem(PREFIX + key)
    return v === null || v === undefined ? fallback : v === "1"
  } catch {
    return fallback
  }
}

export function writeFlag(key: string, value: boolean, store: Store | undefined = browserStore()): void {
  try {
    store?.setItem(PREFIX + key, value ? "1" : "0")
  } catch {
    // without storage the setting only lasts until the next reload
  }
}
