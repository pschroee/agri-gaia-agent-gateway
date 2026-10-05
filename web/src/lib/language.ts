// The user's preferred language according to the browser, for POST /api/chats (language). The agent answers in the
// language of the message; this value only applies if the message reveals none
// (internal/chat/language.go). The same check as in the orchestrator, so that an odd value from the
// browser does not make creating the chat fail.

const BCP47 = /^[A-Za-z]+(-[A-Za-z0-9]+)*$/

/** Checks a language tag (BCP 47, at most 35 characters); invalid or empty: undefined. */
export function validLanguage(tag: string | undefined | null): string | undefined {
  const t = tag?.trim()
  return t && t.length <= 35 && BCP47.test(t) ? t : undefined
}

/** navigator.language, if present and valid. */
export function browserLanguage(nav: { language?: string } | undefined = globalThis.navigator): string | undefined {
  return validLanguage(nav?.language)
}
