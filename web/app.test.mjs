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

function startPlayer(href, stations) {
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
    URL,
    console,
    document: {
      body: new FakeElement(),
      createElement: () => new FakeElement(),
      createRange: () => ({ selectNodeContents() {} }),
      getElementById: (id) => elements[id],
    },
    EventSource: class {
      addEventListener() {}
      close() {}
    },
    fetch: async (url) => {
      fetches.push(url);
      if (url === "/api/stations") {
        return { ok: true, json: async () => ({ stations }) };
      }
      return { ok: true, json: async () => ({ isSilence: true, track: null }) };
    },
    navigator: {},
    window,
  };

  vm.runInNewContext(appSource, context, { filename: "app.js" });
  return { elements, fetches, history, window };
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
