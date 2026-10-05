import type { ExtensionContext } from "@earendil-works/pi-coding-agent";
/** Pi exposes replaced extension contexts as ordinary Errors without a stable code. */
export declare function isStaleExtensionContextError(error: unknown): boolean;
/** Pi throws this from action methods while extensions are still loading, before it binds the runtime. */
export declare function isUnboundExtensionRuntimeError(error: unknown): boolean;
/** Run a synchronous operation against a cached UI context without leaking replacement errors. */
export declare function withCachedUiContext<T>(ctx: ExtensionContext | null | undefined, onStale: () => void, run: (ctx: ExtensionContext) => T): T | undefined;
//# sourceMappingURL=extension-context.d.ts.map