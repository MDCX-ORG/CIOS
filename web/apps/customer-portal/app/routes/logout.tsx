import { redirect } from "react-router";

import { clearSessionCookieHeader } from "~/lib/auth.server";

/** Clear customer mock session and return to login. */
export function loader() {
  return redirect("/login", {
    headers: {
      "Set-Cookie": clearSessionCookieHeader(),
    },
  });
}
