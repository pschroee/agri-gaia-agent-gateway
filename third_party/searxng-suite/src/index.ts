import { type ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { loadConfig } from "./helpers/config";
import { registerSearchTool } from "./tools/search-tool";
import { registerExtractTool } from "./tools/extract-tool";
import { registerConfigCommand } from "./tools/config-command";
import { errorMessage } from "./helpers/error";

export default function (pi: ExtensionAPI) {
  loadConfig();

  pi.on("session_start", (_event, ctx) => {
    loadConfig((err) => {
      ctx.ui.notify(`Config load failed: ${errorMessage(err)}`, "error");
    });
  });

  registerSearchTool(pi);
  registerExtractTool(pi);

  registerConfigCommand(pi);
}
