import { describe, expect, it } from "vitest";
import { pickRandom } from "@/lib/pick-random";

describe("pickRandom", () => {
  const items = [1, 2, 3, 4, 5, 6, 7, 8];

  it("returns exactly the requested number of unique items", () => {
    const picked = pickRandom(items, 6, () => 0.42);
    expect(picked).toHaveLength(6);
    expect(new Set(picked).size).toBe(6);
    picked.forEach((item) => expect(items).toContain(item));
  });

  it("returns the whole list when it is smaller than the requested count", () => {
    expect(pickRandom([1, 2], 6, () => 0.5)).toEqual([1, 2]);
  });

  it("returns an empty list for a non-positive count", () => {
    expect(pickRandom(items, 0)).toEqual([]);
  });

  it("does not mutate the input", () => {
    const source = [...items];
    pickRandom(source, 3, () => 0.7);
    expect(source).toEqual(items);
  });
});
