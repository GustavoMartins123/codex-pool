// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { downloadUsageExport } from "./api";

const originalCreateObjectURL = URL.createObjectURL;
const originalRevokeObjectURL = URL.revokeObjectURL;
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  URL.createObjectURL = originalCreateObjectURL;
  URL.revokeObjectURL = originalRevokeObjectURL;
});

function stubBlobDownload() {
  const clicks: HTMLAnchorElement[] = [];
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) { clicks.push(this); });
  URL.createObjectURL = vi.fn(() => "blob:usage-export");
  URL.revokeObjectURL = vi.fn();
  return clicks;
}

it("requests the console export with window, format and principal and downloads the named attachment", async () => {
  const clicks = stubBlobDownload();
  const fetchMock = vi.fn(async () => new Response("observed_at,principal_id\n", { status: 200, headers: { "Content-Disposition": 'attachment; filename="usage-export-member-1-20261116T120000Z.csv"' } }));
  vi.stubGlobal("fetch", fetchMock);
  await downloadUsageExport("console", { hours: 720, format: "csv", principal: "member-1" });
  expect(fetchMock).toHaveBeenCalledWith("/api/console/usage-export?hours=720&format=csv&principal=member-1", { cache: "no-store", credentials: "same-origin" });
  expect(clicks).toHaveLength(1);
  expect(clicks[0].download).toBe("usage-export-member-1-20261116T120000Z.csv");
  expect(clicks[0].href).toBe("blob:usage-export");
  expect(URL.revokeObjectURL).toHaveBeenCalledWith("blob:usage-export");
});

it("never sends a principal filter on the member export", async () => {
  stubBlobDownload();
  const fetchMock = vi.fn(async () => new Response('{"rows":[]}', { status: 200, headers: { "Content-Disposition": 'attachment; filename="usage-export-me.json"' } }));
  vi.stubGlobal("fetch", fetchMock);
  await downloadUsageExport("me", { hours: 168, format: "json", principal: "someone-else" });
  expect(fetchMock).toHaveBeenCalledWith("/api/me/usage-export?hours=168&format=json", { cache: "no-store", credentials: "same-origin" });
});

it("surfaces the backend error instead of downloading a broken file", async () => {
  const clicks = stubBlobDownload();
  vi.stubGlobal("fetch", vi.fn(async () => new Response('{"error":"analytics query failed"}', { status: 500 })));
  await expect(downloadUsageExport("me", { hours: 168, format: "csv" })).rejects.toThrow("analytics query failed");
  expect(clicks).toHaveLength(0);
});