/**
 * Stille Anmeldung über die Plattform (AGW_AUTH_MODE=oidc): Meldet die API 401 mit `login`, navigiert
 * die UI einmal zu `oidc/login?prompt=none` (unter einem Pfadpräfix etwa `/agent/oidc/login`). Ein Merker in sessionStorage verhindert eine Schleife,
 * wenn Keycloak niemanden kennt; dann zeigt die UI „nicht angemeldet“.
 */
export const LOGIN_FLAG = "agw_oidc_tried"
/** Nach dieser Zeit darf die UI es erneut versuchen (etwa nach der Anmeldung in einem anderen Tab). */
export const LOGIN_RETRY_MS = 60_000

type Store = Pick<Storage, "getItem" | "setItem" | "removeItem">

/**
 * Sagt, ob `login` aus einer 401-Antwort auf die eigene Anmeldung zeigt: relativ (`oidc/login`) oder als
 * Pfad auf diesem Host, auch unter einem Präfix (`/oidc/login`, `/agent/oidc/login`). Fremde Adressen
 * (`//host/…`, `https://…`) nicht.
 */
export function isLoginPath(login: string): boolean {
  return /^(?:(?:\/[A-Za-z0-9_~-][A-Za-z0-9._~-]*)*\/)?oidc\/login$/.test(login)
}

/** Ziel der stillen Anmeldung; danach geht es zur aktuellen Seite zurück (Pfad, Abfrage, Anker). */
export function silentLoginUrl(loginPath: string, loc: { pathname: string; search: string; hash: string }): string {
  const ret = loc.pathname + loc.search + loc.hash
  return `${loginPath}?prompt=none&return=${encodeURIComponent(ret)}`
}

/** Sagt, ob die UI jetzt still anmelden darf, und merkt sich den Versuch. */
export function claimSilentLogin(store: Store | undefined, now: number): boolean {
  try {
    const last = Number(store?.getItem(LOGIN_FLAG) ?? 0)
    if (last && now - last < LOGIN_RETRY_MS) return false
    store?.setItem(LOGIN_FLAG, String(now))
    return true
  } catch {
    return false // ohne Speicher lieber nicht navigieren als endlos
  }
}

/** Nach erfolgreicher Anmeldung den Merker löschen. */
export function clearSilentLogin(store: Store | undefined) {
  try {
    store?.removeItem(LOGIN_FLAG)
  } catch {
    // egal
  }
}

/** Kompakte Ansicht für das Seitenpanel der Plattform (`?embed=1`). */
export function isEmbed(search: string): boolean {
  return new URLSearchParams(search).get("embed") === "1"
}
