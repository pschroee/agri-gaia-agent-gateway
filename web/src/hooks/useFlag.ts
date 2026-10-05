import { useState } from "react"
import { readFlag, writeFlag } from "@/lib/prefs"

/** Schalter, den sich der Browser merkt. */
export function useFlag(key: string, fallback: boolean): [boolean, (v: boolean) => void] {
  const [value, setValue] = useState(() => readFlag(key, fallback))
  const set = (v: boolean) => {
    setValue(v)
    writeFlag(key, v)
  }
  return [value, set]
}
