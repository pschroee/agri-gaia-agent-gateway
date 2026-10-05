/**
 * Silent login through the platform (AGW_AUTH_MODE=oidc): if the API reports 401 with `login`, the UI
 * navigates once to `oidc/login?prompt=none` (under a path prefix e.g. `/agent/oidc/login`). A flag in sessionStorage prevents a loop
 * when Keycloak knows nobody; then the UI shows "not logged in".
 */
export const LOGIN_FLAG = "agw_oidc_tried"
/** After this time the UI may try again (e.g. after logging in in another tab). */
export const LOGIN_RETRY_MS = 60_000

type Store = Pick<Storage, "getItem" | "setItem" | "removeItem">

/**
 * Tells whether `login` from a 401 response points to our own login: relative (`oidc/login`) or as a
 * path on this host, also under a prefix (`/oidc/login`, `/agent/oidc/login`). Foreign addresses
 * (`//host/…`, `https://…`) do not.
 */
export function isLoginPath(login: string): boolean {
  return /^(?:(?:\/[A-Za-z0-9_~-][A-Za-z0-9._~-]*)*\/)?oidc\/login$/.test(login)
}

/** Target of the silent login; afterwards it returns to the current page (path, query, hash). */
export function silentLoginUrl(loginPath: string, loc: { pathname: string; search: string; hash: string }): string {
  const ret = loc.pathname + loc.search + loc.hash
  return `${loginPath}?prompt=none&return=${encodeURIComponent(ret)}`
}

/** Tells whether the UI may log in silently now, and remembers the attempt. */
export function claimSilentLogin(store: Store | undefined, now: number): boolean {
  try {
    const last = Number(store?.getItem(LOGIN_FLAG) ?? 0)
    if (last && now - last < LOGIN_RETRY_MS) return false
    store?.setItem(LOGIN_FLAG, String(now))
    return true
  } catch {
    return false // without storage, better not navigate than loop endlessly
  }
}

/** Clear the flag after a successful login. */
export function clearSilentLogin(store: Store | undefined) {
  try {
    store?.removeItem(LOGIN_FLAG)
  } catch {
    // doesn't matter
  }
}

/** Compact view for the platform's side panel (`?embed=1`). */
export function isEmbed(search: string): boolean {
  return new URLSearchParams(search).get("embed") === "1"
}
