/** A request a recording fetch received. */
export interface Recorded {
  method: string;
  url: URL;
  headers: Headers;
  body: string;
}

/** A fetch that records each request and answers with `reply`'s JSON, as Stripe's API would. */
export function recordingFetch(reply: (request: Recorded) => unknown) {
  const requests: Recorded[] = [];
  const fetcher = async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = new Request(input, init);
    const recorded = { method: request.method, url: new URL(request.url), headers: request.headers, body: await request.text() };
    requests.push(recorded);
    return Response.json(reply(recorded), { headers: { "request-id": "req_test" } });
  };
  return { fetcher, requests };
}
