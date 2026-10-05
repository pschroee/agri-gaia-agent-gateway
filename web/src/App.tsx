import { useEffect, useState } from "react"
import { api } from "@/api/client"
import type { Me } from "@/api/types"
import { PendingApprovalsMenu } from "@/components/PendingApprovalsMenu"
import { Toaster } from "@/components/ui/sonner"
import { TooltipProvider } from "@/components/ui/tooltip"
import { useHashRoute } from "@/hooks/useHashRoute"
import { useMeta } from "@/hooks/useMeta"
import { usePolling } from "@/hooks/usePolling"
import { clearSilentLogin, isEmbed } from "@/lib/auth"
import { cn } from "@/lib/utils"
import { ChatsView } from "@/views/ChatsView"
import { StatusView } from "@/views/StatusView"

export default function App() {
  const route = useHashRoute()
  const meta = useMeta()
  const approvals = usePolling(api.pendingApprovals, 2000)
  // Der Server meldet 401 mit „nicht angemeldet …“ (siehe internal/api).
  const unauthorized = approvals.error?.startsWith("nicht angemeldet") ?? false
  const [me, setMe] = useState<Me>()
  const [embed] = useState(() => isEmbed(window.location.search))
  useEffect(() => {
    if (unauthorized) return
    api
      .me()
      .then((m) => {
        setMe(m)
        try {
          clearSilentLogin(window.sessionStorage)
        } catch {
          // ohne Speicher
        }
      })
      .catch(() => setMe(undefined))
  }, [unauthorized])
  // Ohne Antwort von /api/me gilt: oidc, wenn der Server einen Anmeldepfad nennt (siehe api/client).
  const oidc = me?.mode === "oidc" || approvals.error === "nicht angemeldet"

  return (
    <TooltipProvider>
      <div className="flex h-dvh flex-col bg-background text-foreground">
        {unauthorized &&
          (oidc ? (
            <div role="alert" className="border-b border-amber-300 bg-amber-100 px-4 py-2 text-sm text-amber-950">
              Nicht angemeldet. Bitte in der Plattform anmelden.{" "}
              <a className="underline underline-offset-2" href="oidc/login" target="_blank" rel="noopener">
                Anmelden
              </a>
            </div>
          ) : (
            <div role="alert" className="border-b border-amber-300 bg-amber-100 px-4 py-2 text-sm text-amber-950">
              Nicht angemeldet. Bitte den Anmeldelink öffnen, den <code className="font-mono">./dev.sh start</code>{" "}
              ausgibt (<code className="font-mono">/login?token=…</code>).
            </div>
          ))}
        <header className={cn("flex shrink-0 items-center border-b", embed ? "h-9 gap-2 px-2" : "h-11 gap-3 px-3 sm:gap-6 sm:px-4")}>
          <span className="text-sm font-semibold tracking-tight">
            {embed ? (
              "Agent"
            ) : (
              <>
                agw<span className="hidden sm:inline"> <span className="text-muted-foreground">·</span> PoC-Orchestrator</span>
              </>
            )}
          </span>
          {!embed && (
            <nav className="flex items-center gap-1 text-sm">
              <NavLink href="#/chats" active={route.view === "chats"}>
                Chats
              </NavLink>
              <NavLink href="#/status" active={route.view === "status"}>
                Status
              </NavLink>
            </nav>
          )}
          <PendingApprovalsMenu approvals={approvals.data ?? []} />
          {me?.mode === "oidc" && (
            <span className="ml-auto flex min-w-0 items-center gap-1 text-xs text-muted-foreground" title={me.name || me.username}>
              <span className="truncate">{me.username}</span>
            </span>
          )}
        </header>
        {route.view === "status" && !embed ? (
          <StatusView meta={meta} approvals={approvals.data ?? []} onApprovalsChanged={() => void approvals.reload()} />
        ) : (
          <ChatsView chatId={route.view === "chats" ? route.chatId : undefined} runId={route.view === "chats" ? route.runId : undefined} meta={meta} embed={embed} />
        )}
      </div>
      <Toaster position="bottom-right" richColors />
    </TooltipProvider>
  )
}

function NavLink({ href, active, children }: { href: string; active: boolean; children: React.ReactNode }) {
  return (
    <a
      href={href}
      className={cn(
        "rounded-md px-2.5 py-1 hover:bg-muted",
        active ? "bg-muted font-medium text-foreground" : "text-muted-foreground",
      )}
    >
      {children}
    </a>
  )
}
