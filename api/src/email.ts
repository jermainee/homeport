export class EmailUnavailableError extends Error {
  constructor() {
    super("Email delivery is not configured");
    this.name = "EmailUnavailableError";
  }
}

export interface SignInEmail {
  to: string;
  link: string;
  matchCode: string;
}

/**
 * Delivers the sign-in link. Only log mode exists so far, and it works only when
 * ENVIRONMENT is "dev", so a misconfigured production Worker never writes
 * sign-in links to its logs.
 */
export function sendSignInEmail(env: Env, email: SignInEmail): Promise<void> {
  if (env.EMAIL_MODE === "log" && env.ENVIRONMENT === "dev") {
    console.log(
      JSON.stringify({
        event: "magic_link_email",
        to: email.to,
        link: email.link,
        match_code: email.matchCode,
      }),
    );
    return Promise.resolve();
  }
  return Promise.reject(new EmailUnavailableError());
}
