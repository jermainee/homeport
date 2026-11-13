export interface SignInEmail {
  to: string;
  token: string;
  matchCode: string;
}

export interface SignInMailer {
  send(email: SignInEmail): Promise<void>;
}

/**
 * The way sign-in links go out in this environment, or null when they cannot
 * be sent. Check it before creating anything, so a 503 leaves no state behind.
 *
 * Only log mode exists so far, and it works only when ENVIRONMENT is "dev", so
 * a misconfigured production Worker never writes sign-in links to its logs.
 * Without a valid PUBLIC_BASE_URL nothing is sent.
 */
export function signInMailer(env: Env): SignInMailer | null {
  const baseUrl = httpOrigin(env.PUBLIC_BASE_URL);
  if (!baseUrl || env.EMAIL_MODE !== "log" || env.ENVIRONMENT !== "dev") return null;
  return {
    send(email) {
      const link = new URL("/auth/confirm", baseUrl);
      link.searchParams.set("token", email.token);
      console.log(
        JSON.stringify({
          event: "magic_link_email",
          to: email.to,
          link: link.href,
          match_code: email.matchCode,
        }),
      );
      return Promise.resolve();
    },
  };
}

function httpOrigin(value: string): URL | null {
  if (!URL.canParse(value)) return null;
  const url = new URL(value);
  return url.protocol === "https:" || url.protocol === "http:" ? url : null;
}
