import { Badge } from "@/components/ui/badge"
import { Command, CommandGroup, CommandItem, CommandList } from "@/components/ui/command"
import { Popover, PopoverAnchor, PopoverContent } from "@/components/ui/popover"
import type { SlashState } from "@/hooks/useSlashCommands"

const sourceLabel: Record<string, string> = {
  builtin: "built-in",
  extension: "extension",
  prompt: "template",
  skill: "skill",
}

/** Command list above the input field; focus stays in the input field. */
export function SlashCommandPopover({ state, children }: { state: SlashState; children: React.ReactNode }) {
  const current = state.items[state.active]
  const key = (i: { command: { name: string }; option?: { value: string } }) =>
    i.option ? `${i.command.name} ${i.option.value}` : i.command.name
  return (
    <Popover open={state.open} onOpenChange={(o) => !o && state.close()}>
      <PopoverAnchor asChild>
        <div>{children}</div>
      </PopoverAnchor>
      <PopoverContent
        side="top"
        align="start"
        className="w-(--radix-popover-trigger-width) max-w-[calc(100vw-2rem)] p-0"
        onOpenAutoFocus={(e) => e.preventDefault()}
        onCloseAutoFocus={(e) => e.preventDefault()}
      >
        <Command shouldFilter={false} value={current ? key(current) : ""} loop>
          <CommandList className="max-h-64">
            <CommandGroup
              heading={
                state.argsOf
                  ? `/${state.argsOf.name}: choose a value (↑↓, Enter/Tab to accept, Esc to close)`
                  : "Commands (↑↓ to choose, Enter/Tab to accept, Esc to close)"
              }
            >
              {state.items.map((item, i) =>
                item.option ? (
                  <CommandItem
                    key={key(item)}
                    value={key(item)}
                    onMouseEnter={() => state.setActive(i)}
                    onSelect={() => state.pick(item)}
                  >
                    <div className="min-w-0 flex-1">
                      <div className="font-mono text-xs">{item.option.value}</div>
                      {item.option.label && <div className="truncate text-xs text-muted-foreground">{item.option.label}</div>}
                    </div>
                    {item.option.current && (
                      <Badge variant="outline" className="shrink-0 text-[10px]">
                        current
                      </Badge>
                    )}
                  </CommandItem>
                ) : (
                <CommandItem
                  key={item.command.name}
                  value={item.command.name}
                  onMouseEnter={() => state.setActive(i)}
                  onSelect={() => state.pick(item)}
                  className="items-start"
                >
                  <div className="min-w-0 flex-1">
                    <div className="font-mono text-xs">
                      /{item.command.name}
                      {item.command.args && <span className="ml-1 text-muted-foreground">{item.command.args}</span>}
                    </div>
                    {item.command.description && (
                      <div className="truncate text-xs text-muted-foreground">{item.command.description}</div>
                    )}
                  </div>
                  <Badge variant="outline" className="shrink-0 text-[10px]">
                    {sourceLabel[item.command.source] ?? item.command.source}
                  </Badge>
                </CommandItem>
                ),
              )}
            </CommandGroup>
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  )
}
