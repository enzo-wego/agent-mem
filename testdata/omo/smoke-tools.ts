import { appendFileSync } from "node:fs";
import { Type } from "typebox";

export default function (pi) {
  pi.registerTool({
    name: "smoke_probe",
    label: "Synthetic smoke probe",
    description: "Return or throw a fixture value; has no side effects.",
    parameters: Type.Object({ outcome: Type.Union([Type.Literal("ok"), Type.Literal("fail")]) }),
    async execute(_id, { outcome }) {
      if (outcome === "fail") throw new Error("OMO_SMOKE_TOOL_FAILURE");
      return { content: [{ type: "text", text: "OMO_SMOKE_TOOL_SUCCESS" }], details: {} };
    },
  });
  // This fixture observes lifecycle events independently; it never calls agent-mem.
  for (const type of ["agent_settled", "session_shutdown"]) {
    pi.on(type, () => appendFileSync(process.env.OMO_SMOKE_LIFECYCLE!, `${type}\n`));
  }
  pi.registerCommand("smoke-quit", {
    description: "Shut down the isolated smoke session normally",
    async handler(_args, ctx) {
      ctx.shutdown();
    },
  });
}
