// Kleine Ansichtseinstellungen je Browser (etwa: Seitenleiste ein/aus). Der Speicher kann fehlen oder
// gesperrt sein (privates Fenster); dann gilt der Standard und nichts bricht.

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
    // ohne Speicher gilt die Einstellung nur bis zum Neuladen
  }
}
