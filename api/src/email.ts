export class EmailUnavailableError extends Error {
  constructor() {
    super("Email delivery is not configured");
    this.name = "EmailUnavailableError";
  }
}

export interface SignInEmail {
  to: string;
  token: string;
  matchCode: string;
}

/**
 * Delivers the sign-in link. Only log mode exists so far, and it works only when
 * ENVIRONMENT is "dev", so a misconfigured production Worker never writes
 * sign-in links to its logs. Without a valid PUBLIC_BASE_URL nothing is sent.
 */
export function sendSignInEmail(env: Env, email: SignInEmail): Promise<void> {
  const link = signInLink(env.PUBLIC_BASE_URL, email.token);
  if (link && env.EMAIL_MODE === "log" && env.ENVIRONMENT === "dev") {
    console.log(
      JSON.stringify({
        event: "magic_link_email",
        to: email.to,
        link,
        match_code: email.matchCode,
      }),
    );
    return Promise.resolve();
  }
  return Promise.reject(new EmailUnavailableError());
}

/** The emailed link, or null when PUBLIC_BASE_URL is not an http(s) origin. */
function signInLink(baseUrl: string, token: string): string | null {
  if (!URL.canParse(baseUrl)) return null;
  const link = new URL("/auth/confirm", baseUrl);
  if (link.protocol !== "https:" && link.protocol !== "http:") return null;
  link.searchParams.set("token", token);
  return link.href;
}
