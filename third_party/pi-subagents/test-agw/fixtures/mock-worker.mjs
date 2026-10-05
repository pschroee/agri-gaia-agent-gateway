// Mock for PI_SUBAGENTS_WORKFLOW_WORKER: logs every instantiation to the file
// from AGW_MOCK_LOG and then aborts, so that runWorkflowScript ends at once with a
// recognisable error without starting a real worker.
import { appendFileSync } from "node:fs";

export const MOCK_MARKER = "agw-mock";

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
