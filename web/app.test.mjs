import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const appSource = await readFile(new URL("./app.js", import.meta.url), "utf8");

class FakeElement {
  constructor() {
    this.attributes = new Map();
    this.children = [];
    this.listeners = new Map();
    this.paused = true;
    this.style = {};
    this.textContent = "";
    this.value = "";
  }

  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) || [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }

  dispatch(type) {
    for (const listener of this.listeners.get(type) || []) listener({ type });
  }

  pause() {
    this.paused = true;
  }

  removeAttribute(name) {
    this.attributes.delete(name);
  }

  replaceChildren(...children) {
    this.children = children;
  }

  setAttribute(name, value) {
    this.attributes.set(name, String(value));
  }
}

function startPlayer(href, stations, options = {}) {
  const apiBaseUrl = options.apiBaseUrl || "";
  const now = options.now || { isSilence: true, track: null };
  const ids = [
    "audio",
    "station",
    "play",
    "title",
    "artist",
    "status",
    "cover",
    "coverFallback",
    "streamUrl",
    "streamCommand",
    "copyStatus",
  ];
  const elements = Object.fromEntries(ids.map((id) => [id, new FakeElement()]));
  const fetches = [];
  const eventSources = [];
  const history = [];
  const window = {
    location: { href },
    history: {
      replaceState(_state, _title, nextURL) {
        window.location.href = new URL(nextURL, window.location.href).toString();
        history.push(window.location.href);
      },
    },
  };

  const context = {
    RAYDIO_CONFIG: { apiBaseUrl },
    URL,
    console,
    document: {
      body: new FakeElement(),
      createElement: () => new FakeElement(),
      createRange: () => ({ selectNodeContents() {} }),
      getElementById: (id) => elements[id],
    },
    EventSource: class {
      constructor(url) {
        eventSources.push(url);
      }

      addEventListener() {}
      close() {}
    },
    fetch: async (url) => {
      fetches.push(url);
      if (String(url).endsWith("/api/stations")) {
        return { ok: true, json: async () => ({ stations }) };
      }
      return { ok: true, json: async () => now };
    },
    navigator: {},
    window,
  };

  vm.runInNewContext(appSource, context, { filename: "app.js" });
  return { elements, eventSources, fetches, history, window };
}

async function settle() {
  for (let i = 0; i < 4; i += 1) {
    await new Promise((resolve) => setImmediate(resolve));
  }
}

const stationFixtures = [
  { alias: "foo", uuid: "11111111-1111-1111-1111-111111111111" },
  { alias: "all", uuid: "00000000-0000-0000-0000-000000000000" },
  { alias: "bar", uuid: "22222222-2222-2222-2222-222222222222" },
];

test("puts all first and makes it the URL-backed default", async () => {
  const player = startPlayer("https://example.test/player?theme=dark#current", stationFixtures);
  await settle();

  assert.deepEqual(
    player.elements.station.children.map((option) => option.value),
    ["all", "foo", "bar"],
  );
  assert.equal(player.elements.station.value, "all");
  assert.equal(player.elements.audio.src, "/radio/all");
  assert.deepEqual(player.fetches, ["/api/stations", "/radio/all/api/now"]);
  assert.deepEqual(player.eventSources, ["/radio/all/api/events"]);
  assert.equal(player.elements.streamUrl.textContent, "https://example.test/radio/all");

  const currentURL = new URL(player.window.location.href);
  assert.equal(currentURL.searchParams.get("raydio"), "all");
  assert.equal(currentURL.searchParams.get("theme"), "dark");
  assert.equal(currentURL.hash, "#current");
});

test("selects a valid station from the raydio query", async () => {
  const player = startPlayer("https://example.test/?raydio=foo", stationFixtures);
  await settle();

  assert.equal(player.elements.station.value, "foo");
  assert.equal(player.elements.audio.src, "/radio/foo");
  assert.ok(player.fetches.includes("/radio/foo/api/now"));
  assert.deepEqual(player.history, []);
});

test("accepts a station UUID without rewriting it", async () => {
  const uuid = stationFixtures[0].uuid;
  const player = startPlayer(`https://example.test/?raydio=${uuid}`, stationFixtures);
  await settle();

  assert.equal(player.elements.station.value, "foo");
  assert.equal(player.elements.audio.src, "/radio/foo");
  assert.equal(new URL(player.window.location.href).searchParams.get("raydio"), uuid);
  assert.deepEqual(player.history, []);
});

test("falls back invalid values to all and keeps the URL in sync", async () => {
  const player = startPlayer("https://example.test/?raydio=missing&theme=dark#current", stationFixtures);
  await settle();

  assert.equal(player.elements.station.value, "all");
  assert.equal(new URL(player.window.location.href).searchParams.get("raydio"), "all");

  player.elements.station.value = "bar";
  player.elements.station.dispatch("change");
  await settle();

  const currentURL = new URL(player.window.location.href);
  assert.equal(player.elements.station.value, "bar");
  assert.equal(currentURL.searchParams.get("raydio"), "bar");
  assert.equal(currentURL.searchParams.get("theme"), "dark");
  assert.equal(currentURL.hash, "#current");
});

test("uses the configured API base from a project Pages URL", async () => {
  const apiBaseUrl = "https://api.example.test/raydio-api/";
  const player = startPlayer(
    "https://foo.github.io/raydio/?theme=dark#current",
    stationFixtures,
    {
      apiBaseUrl,
      now: {
        isSilence: false,
        track: {
          id: "track-1",
          title: "Track",
          artist: "Artist",
          coverUrl: "/radio/all/covers/track-1",
        },
      },
    },
  );
  await settle();

  const normalizedBase = "https://api.example.test/raydio-api";
  assert.deepEqual(player.fetches, [
    `${normalizedBase}/api/stations`,
    `${normalizedBase}/radio/all/api/now`,
  ]);
  assert.equal(player.elements.audio.src, `${normalizedBase}/radio/all`);
  assert.deepEqual(player.eventSources, [`${normalizedBase}/radio/all/api/events`]);
  assert.equal(
    player.elements.cover.src,
    `${normalizedBase}/radio/all/covers/track-1?v=track-1`,
  );
  assert.equal(player.elements.streamUrl.textContent, `${normalizedBase}/radio/all`);
  assert.ok(player.elements.streamCommand.textContent.includes(`'${normalizedBase}/radio/all'`));

  const currentURL = new URL(player.window.location.href);
  assert.equal(currentURL.origin, "https://foo.github.io");
  assert.equal(currentURL.pathname, "/raydio/");
  assert.equal(currentURL.searchParams.get("raydio"), "all");
  assert.equal(currentURL.searchParams.get("theme"), "dark");
  assert.equal(currentURL.hash, "#current");
});
