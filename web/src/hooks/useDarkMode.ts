import { useSyncExternalStore } from "react"

/** Ob die Oberfläche dunkel ist (Klasse `dark` am <html>, wie in index.css vorgesehen). */
const isDark = () => typeof document !== "undefined" && document.documentElement.classList.contains("dark")

function subscribe(onChange: () => void) {
  const obs = new MutationObserver(onChange)
  obs.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] })
  return () => obs.disconnect()
}

export function useDarkMode(): boolean {
  return useSyncExternalStore(subscribe, isDark, () => false)
}
