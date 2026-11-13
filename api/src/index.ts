import { createApp } from "./app";
import { prune } from "./prune";

const app = createApp();

export default {
  fetch: app.fetch,

  /** The cron trigger in wrangler.jsonc: deletes rows nothing needs any more. */
  scheduled(controller, env, ctx) {
    ctx.waitUntil(
      prune(env.DB, controller.scheduledTime).then((deleted) => {
        console.log(JSON.stringify({ event: "prune", deleted }));
      }),
    );
  },
} satisfies ExportedHandler<Env>;
