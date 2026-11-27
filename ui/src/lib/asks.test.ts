import { QueryClient } from "@tanstack/react-query";
import { expect, it } from "vitest";
import { lastAsk, watchAnswers } from "./asks";

it("counts a question's first answer as an ask, and its refreshes not", async () => {
  const client = new QueryClient();
  const stop = watchAnswers(client);
  const fetch = () => client.fetchQuery({ queryKey: ["snapshot"], queryFn: async () => 1, staleTime: 0 });
  const before = lastAsk();
  await fetch();
  const answered = lastAsk();
  expect(answered).toBeGreaterThan(before);
  await fetch();
  expect(lastAsk()).toBe(answered);
  stop();
});
