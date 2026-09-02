import { redirect } from "react-router";

import type { Route } from "./+types/_index";
import { getSession } from "~/lib/auth.server";

/** Home → status when logged in, else login. */
export async function loader({ request }: Route.LoaderArgs) {
  const s = await getSession(request);
  if (s) return redirect("/status");
  return redirect("/login");
}
