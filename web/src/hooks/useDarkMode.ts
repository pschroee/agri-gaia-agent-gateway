import { useSyncExternalStore } from "react"

/** Whether the UI is dark (class `dark` on <html>, as intended in index.css). */
const isDark = () => typeof document !== "undefined" && document.documentElement.classList.contains("dark")

function subscribe(onChange: () => void) {
  const obs = new MutationObserver(onChange)
  obs.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] })
  return () => obs.disconnect()
}

export function useDarkMode(): boolean {
  return useSyncExternalStore(subscribe, isDark, () => false)
}
