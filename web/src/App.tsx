import { api } from "@/api/client"
import { PendingApprovalsMenu } from "@/components/PendingApprovalsMenu"
import { Toaster } from "@/components/ui/sonner"
import { TooltipProvider } from "@/components/ui/tooltip"
import { useHashRoute } from "@/hooks/useHashRoute"
import { useMeta } from "@/hooks/useMeta"
import { usePolling } from "@/hooks/usePolling"
import { cn } from "@/lib/utils"
import { ChatsView } from "@/views/ChatsView"
import { StatusView } from "@/views/StatusView"

export default function App() {
  const route = useHashRoute()
  const meta = useMeta()
  const approvals = usePolling(api.pendingApprovals, 2000)
  // Der Server meldet 401 mit „nicht angemeldet: …“ (siehe internal/api).
  const unauthorized = approvals.error?.startsWith("nicht angemeldet") ?? false

  return (
    <TooltipProvider>
      <div className="flex h-dvh flex-col bg-background text-foreground">
        {unauthorized && (
          <div role="alert" className="border-b border-amber-300 bg-amber-100 px-4 py-2 text-sm text-amber-950">
            Nicht angemeldet. Bitte den Anmeldelink öffnen, den <code className="font-mono">./dev.sh start</code>{" "}
            ausgibt (<code className="font-mono">/login?token=…</code>).
          </div>
        )}
        <header className="flex h-11 shrink-0 items-center gap-3 border-b px-3 sm:gap-6 sm:px-4">
          <span className="text-sm font-semibold tracking-tight">
            agw<span className="hidden sm:inline"> <span className="text-muted-foreground">·</span> PoC-Orchestrator</span>
          </span>
          <nav className="flex items-center gap-1 text-sm">
            <NavLink href="#/chats" active={route.view === "chats"}>
              Chats
            </NavLink>
            <NavLink href="#/status" active={route.view === "status"}>
              Status
            </NavLink>
          </nav>
          <PendingApprovalsMenu approvals={approvals.data ?? []} />
        </header>
        {route.view === "status" ? (
          <StatusView meta={meta} approvals={approvals.data ?? []} onApprovalsChanged={() => void approvals.reload()} />
        ) : (
          <ChatsView chatId={route.chatId} runId={route.runId} meta={meta} />
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
