// Bevorzugte Sprache des Nutzers laut Browser, für POST /api/chats (language). Der Agent antwortet in der
// Sprache der Nachricht; diese Angabe gilt nur, wenn die Nachricht keine erkennen lässt
// (internal/chat/language.go). Dieselbe Prüfung wie im Orchestrator, damit eine seltsame Angabe des
// Browsers das Anlegen nicht scheitern lässt.

const BCP47 = /^[A-Za-z]+(-[A-Za-z0-9]+)*$/

/** Prüft eine Sprachangabe (BCP 47, höchstens 35 Zeichen); ungültig oder leer: undefined. */
export function validLanguage(tag: string | undefined | null): string | undefined {
  const t = tag?.trim()
  return t && t.length <= 35 && BCP47.test(t) ? t : undefined
}

/** navigator.language, falls vorhanden und gültig. */
export function browserLanguage(nav: { language?: string } | undefined = globalThis.navigator): string | undefined {
  return validLanguage(nav?.language)
}
