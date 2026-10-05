// Attrappe für PI_SUBAGENTS_WORKFLOW_WORKER: protokolliert jede Instanziierung in die Datei
// aus AGW_MOCK_LOG und bricht dann ab, damit runWorkflowScript sofort mit einem
// erkennbaren Fehler endet, ohne einen echten Worker zu starten.
import { appendFileSync } from "node:fs";

export const MOCK_MARKER = "agw-attrappe";

export class Worker {
	constructor(source, options) {
		appendFileSync(
			process.env.AGW_MOCK_LOG,
			`${JSON.stringify({
				module: "mock-worker.mjs",
				sourceIsString: typeof source === "string",
				eval: options?.eval === true,
				hasAcornPath: typeof options?.workerData?.acornPath === "string",
			})}\n`,
		);
		throw new Error(MOCK_MARKER);
	}
}
