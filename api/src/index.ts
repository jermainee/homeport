import { createApp } from "./app";
import { RECOUNT_CRON, recountLineRegions } from "./lines/regions";
import { prune } from "./prune";

const app = createApp();

export default {
  fetch: app.fetch,

  /**
   * The cron triggers in wrangler.jsonc: every minute, recounts the free
   * lines in each region; hourly, deletes rows nothing needs any more.
   */
  scheduled(controller, env, ctx) {
    if (controller.cron === RECOUNT_CRON) {
      ctx.waitUntil(recountLineRegions(env.DB, controller.scheduledTime));
      return;
    }
    ctx.waitUntil(
      prune(env.DB, controller.scheduledTime).then((deleted) => {
        console.log(JSON.stringify({ event: "prune", deleted }));
      }),
    );
  },
} satisfies ExportedHandler<Env>;
