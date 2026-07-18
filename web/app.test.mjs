import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const appSource = await readFile(new URL("./app.js", import.meta.url), "utf8");

class FakeClock {
  constructor() {
    this.nextID = 1;
    this.now = 0;
    this.timers = new Map();
  }

  setTimeout(callback, delay = 0) {
    const id = this.nextID;
    this.nextID += 1;
    this.timers.set(id, { callback, due: this.now + Number(delay) });
    return id;
  }

  clearTimeout(id) {
    this.timers.delete(id);
  }

  pendingDelays() {
    return [...this.timers.values()].map((timer) => timer.due - this.now).sort((a, b) => a - b);
  }

  async runNext() {
    const next = [...this.timers.entries()].sort(
      ([leftID, left], [rightID, right]) => left.due - right.due || leftID - rightID,
    )[0];
    assert.ok(next, "expected a pending timer");
    const [id, timer] = next;
    this.timers.delete(id);
    this.now = timer.due;
    timer.callback();
    await settle();
  }
}

class FakeElement {
  constructor() {
    this.attributes = new Map();
    this.children = [];
    this.listeners = new Map();
    this.pauseCalls = 0;
    this.paused = true;
    this.playCalls = 0;
    this.playError = null;
    this.style = {};
    this.textContent = "";
    this.value = "";
  }

  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) || [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }

  appendChild(child) {
    this.children.push(child);
    return child;
  }

  dispatch(type, init = {}) {
    const event = { type, preventDefault() {}, ...init };
    return (this.listeners.get(type) || []).map((listener) => listener(event));
  }

  pause() {
    this.pauseCalls += 1;
    const wasPaused = this.paused;
    this.paused = true;
    if (!wasPaused) this.dispatch("pause");
  }

  async play() {
    this.playCalls += 1;
    if (this.playError) throw this.playError;
    this.paused = false;
    this.dispatch("play");
  }

  remove() {}

  removeAttribute(name) {
    this.attributes.delete(name);
    if (name === "src") delete this.src;
  }

  replaceChildren(...children) {
    this.children = children;
  }

  select() {}

  setAttribute(name, value) {
    this.attributes.set(name, String(value));
  }
}

class FakeEventSource {
  constructor(url) {
    this.closed = false;
    this.listeners = new Map();
    this.url = String(url);
  }

  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) || [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }

  close() {
    this.closed = true;
  }

  dispatch(type, init = {}) {
    for (const listener of this.listeners.get(type) || []) listener({ type, ...init });
  }
}

function jsonResponse(body, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  };
}

function startPlayer(href, stationFixtures, options = {}) {
  const configEntries = [...(options.configEntries || [options.apiBaseUrl || ""])];
  const now = options.now || { isSilence: true, track: null };
  const clock = options.clock || null;
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
  const eventSources = [];
  const fetches = [];
  const history = [];
  const requests = [];
  const windowListeners = new Map();
  let configIndex = 0;

  const window = {
    location: { href },
    history: {
      replaceState(_state, _title, nextURL) {
        window.location.href = new URL(nextURL, window.location.href).toString();
        history.push(window.location.href);
      },
    },
    addEventListener(type, listener) {
      const listeners = windowListeners.get(type) || [];
      listeners.push(listener);
      windowListeners.set(type, listeners);
    },
    dispatch(type) {
      for (const listener of windowListeners.get(type) || []) listener({ type });
    },
    getSelection() {
      return { addRange() {}, removeAllRanges() {} };
    },
  };

  const fetch = async (url, requestOptions = {}) => {
    const value = String(url);
    fetches.push(value);
    requests.push({ options: requestOptions, url: value });
    const parsed = new URL(value, window.location.href);

    if (parsed.pathname.endsWith("/config.json")) {
      const entry = configEntries[Math.min(configIndex, configEntries.length - 1)];
      configIndex += 1;
      if (entry instanceof Error) throw entry;
      return jsonResponse(typeof entry === "string" ? { apiBaseUrl: entry } : entry);
    }

    if (options.failRequest?.(value, requestOptions)) throw new Error("network failure");
    const overridden = await options.requestHandler?.(value, requestOptions);
    if (overridden !== undefined) return overridden;

    if (parsed.pathname.endsWith("/api/stations")) {
      return jsonResponse({ stations: stationFixtures });
    }
    return jsonResponse(now);
  };

  const context = {
    AbortController,
    Date,
    URL,
    clearTimeout: clock ? clock.clearTimeout.bind(clock) : clearTimeout,
    console,
    document: {
      baseURI: new URL(".", href).toString(),
      body: new FakeElement(),
      createElement: () => new FakeElement(),
      createRange: () => ({ selectNodeContents() {} }),
      execCommand: () => true,
      getElementById: (id) => elements[id],
    },
    EventSource: class extends FakeEventSource {
      constructor(url) {
        super(url);
        eventSources.push(this);
      }
    },
    fetch,
    navigator: {},
    setTimeout: clock ? clock.setTimeout.bind(clock) : setTimeout,
    window,
  };

  vm.runInNewContext(appSource, context, { filename: "app.js" });
  return { clock, elements, eventSources, fetches, history, requests, window };
}

async function settle() {
  for (let i = 0; i < 8; i += 1) {
    await new Promise((resolve) => setImmediate(resolve));
  }
}

function apiFetches(player) {
  return player.fetches.filter((value) => !new URL(value, player.window.location.href).pathname.endsWith("/config.json"));
}

function eventSourceURLs(player) {
  return player.eventSources.map((source) => source.url);
}

const stationFixtures = [
  { alias: "foo", uuid: "11111111-1111-1111-1111-111111111111" },
  { alias: "all", uuid: "00000000-0000-0000-0000-000000000000" },
  { alias: "bar", uuid: "22222222-2222-2222-2222-222222222222" },
];

test("loads uncached runtime config, puts all first, and selects it by default", async () => {
  const player = startPlayer("https://example.test/player?theme=dark#current", stationFixtures);
  await settle();

  const configRequest = player.requests[0];
  assert.equal(new URL(configRequest.url).pathname, "/config.json");
  assert.match(new URL(configRequest.url).searchParams.get("request"), /^\d+-1$/);
  assert.equal(configRequest.options.cache, "no-store");
  assert.ok(configRequest.options.signal instanceof AbortSignal);
  assert.deepEqual(
    player.elements.station.children.map((option) => option.value),
    ["all", "foo", "bar"],
  );
  assert.equal(player.elements.station.value, "all");
  assert.equal(player.elements.audio.src, "/radio/all");
  assert.deepEqual(apiFetches(player), ["/api/stations", "/radio/all/api/now"]);
  assert.deepEqual(eventSourceURLs(player), ["/radio/all/api/events"]);
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
  assert.ok(apiFetches(player).includes("/radio/foo/api/now"));
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

test("uses config.json from a project Pages URL", async () => {
  const apiBaseUrl = "https://api.example.test/raydio-api/";
  const player = startPlayer(
    "https://foo.github.io/raydio/?theme=dark#current",
    stationFixtures,
    {
      apiBaseUrl,
      now: {
        isSilence: false,
        track: {
          artist: "Artist",
          coverUrl: "/radio/all/covers/track-1",
          id: "track-1",
          title: "Track",
        },
      },
    },
  );
  await settle();

  assert.equal(new URL(player.requests[0].url).pathname, "/raydio/config.json");
  const normalizedBase = "https://api.example.test/raydio-api";
  assert.deepEqual(apiFetches(player), [
    `${normalizedBase}/api/stations`,
    `${normalizedBase}/radio/all/api/now`,
  ]);
  assert.equal(player.elements.audio.src, `${normalizedBase}/radio/all`);
  assert.deepEqual(eventSourceURLs(player), [`${normalizedBase}/radio/all/api/events`]);
  assert.equal(
    player.elements.cover.src,
    `${normalizedBase}/radio/all/covers/track-1?v=track-1`,
  );
  assert.equal(player.elements.streamUrl.textContent, `${normalizedBase}/radio/all`);
  assert.ok(player.elements.streamCommand.textContent.includes(`'${normalizedBase}/radio/all'`));
});

test("recovers a playing station from a newly deployed base URL", async () => {
  const clock = new FakeClock();
  const oldBase = "https://old.example.test";
  const newBase = "https://new.example.test/raydio";
  const player = startPlayer("https://foo.github.io/raydio/?raydio=foo", stationFixtures, {
    clock,
    configEntries: [oldBase, newBase],
  });
  await settle();

  player.elements.play.dispatch("click");
  await settle();
  assert.equal(player.elements.audio.playCalls, 1);
  const oldEvents = player.eventSources[0];

  player.elements.audio.dispatch("error");
  player.elements.audio.dispatch("error");
  oldEvents.dispatch("error");
  assert.deepEqual(clock.pendingDelays(), [0]);
  await clock.runNext();

  assert.equal(oldEvents.closed, true);
  assert.equal(player.elements.station.value, "foo");
  assert.equal(player.elements.audio.src, `${newBase}/radio/foo`);
  assert.equal(player.elements.audio.playCalls, 2);
  assert.equal(player.elements.play.textContent, "停止");
  assert.equal(player.elements.status.textContent, "playing");
  assert.equal(player.elements.streamUrl.textContent, `${newBase}/radio/foo`);
  assert.equal(player.eventSources.at(-1).url, `${newBase}/radio/foo/api/events`);
  assert.deepEqual(clock.pendingDelays(), []);
});

test("keeps the active base when the newly deployed base is unreachable", async () => {
  const clock = new FakeClock();
  const oldBase = "https://old.example.test";
  const newBase = "https://new.example.test";
  const player = startPlayer("https://foo.github.io/raydio/", stationFixtures, {
    clock,
    configEntries: [oldBase, newBase],
    failRequest: (url) => url === `${newBase}/api/stations`,
  });
  await settle();

  player.elements.play.dispatch("click");
  await settle();
  const oldEvents = player.eventSources[0];
  player.elements.audio.dispatch("error");
  await clock.runNext();

  assert.equal(oldEvents.closed, false);
  assert.equal(player.elements.audio.src, `${oldBase}/radio/all`);
  assert.equal(player.elements.streamUrl.textContent, `${oldBase}/radio/all`);
  assert.equal(player.elements.status.textContent, "reconnecting");
  assert.deepEqual(clock.pendingDelays(), [2000]);

  await clock.runNext();
  assert.deepEqual(clock.pendingDelays(), [5000]);
  await clock.runNext();
  assert.deepEqual(clock.pendingDelays(), [10000]);
  await clock.runNext();
  assert.deepEqual(clock.pendingDelays(), [30000]);
  await clock.runNext();
  assert.deepEqual(clock.pendingDelays(), [30000]);

  player.window.dispatch("online");
  assert.deepEqual(clock.pendingDelays(), [0]);
});

test("does not resume playback when the user stops during recovery", async () => {
  const clock = new FakeClock();
  const oldBase = "https://old.example.test";
  const newBase = "https://new.example.test";
  const player = startPlayer("https://foo.github.io/raydio/?raydio=bar", stationFixtures, {
    clock,
    configEntries: [oldBase, newBase],
  });
  await settle();

  player.elements.play.dispatch("click");
  await settle();
  player.elements.audio.dispatch("error");
  player.elements.play.dispatch("click");
  assert.equal(player.elements.play.textContent, "播放");
  await clock.runNext();

  assert.equal(player.elements.audio.src, `${newBase}/radio/bar`);
  assert.equal(player.elements.audio.playCalls, 1);
  assert.equal(player.elements.audio.paused, true);
  assert.equal(player.elements.play.textContent, "播放");
});

test("reports play blocked when browser policy rejects automatic resume", async () => {
  const clock = new FakeClock();
  const oldBase = "https://old.example.test";
  const newBase = "https://new.example.test";
  const player = startPlayer("https://foo.github.io/raydio/", stationFixtures, {
    clock,
    configEntries: [oldBase, newBase],
  });
  await settle();

  player.elements.play.dispatch("click");
  await settle();
  player.elements.audio.playError = new Error("autoplay blocked");
  player.elements.audio.dispatch("error");
  await clock.runNext();

  assert.equal(player.elements.audio.src, `${newBase}/radio/all`);
  assert.equal(player.elements.audio.playCalls, 2);
  assert.equal(player.elements.status.textContent, "play blocked");
  assert.equal(player.elements.play.textContent, "播放");
  assert.deepEqual(clock.pendingDelays(), []);
});

test("an unchanged config lets EventSource recover without restarting audio", async () => {
  const clock = new FakeClock();
  const base = "https://api.example.test";
  const player = startPlayer("https://foo.github.io/raydio/", stationFixtures, {
    clock,
    configEntries: [base, base],
  });
  await settle();

  player.elements.play.dispatch("click");
  await settle();
  const source = player.eventSources[0];
  const pauseCalls = player.elements.audio.pauseCalls;
  source.dispatch("error");
  await clock.runNext();

  assert.equal(source.closed, false);
  assert.equal(player.elements.audio.pauseCalls, pauseCalls);
  assert.equal(player.elements.audio.playCalls, 1);
  assert.deepEqual(clock.pendingDelays(), [2000]);

  source.dispatch("open");
  assert.deepEqual(clock.pendingDelays(), []);
  assert.equal(player.elements.status.textContent, "playing");
});

test("rejects unknown config fields and retries with a later valid config", async () => {
  const clock = new FakeClock();
  const player = startPlayer("https://foo.github.io/raydio/", stationFixtures, {
    clock,
    configEntries: [
      { apiBaseUrl: "https://api.example.test", unexpected: true },
      "https://api.example.test",
    ],
  });
  await settle();

  assert.equal(player.elements.status.textContent, "reconnecting");
  assert.deepEqual(clock.pendingDelays(), [0]);
  await clock.runNext();

  assert.equal(player.elements.audio.src, "https://api.example.test/radio/all");
  assert.equal(player.elements.status.textContent, "ready");
  assert.deepEqual(clock.pendingDelays(), []);
});

test("rejects unsafe runtime config URLs before making API requests", async (t) => {
  const invalidValues = [
    "http://api.example.test",
    "https:///missing-host",
    "https://user:secret@api.example.test",
    "https://api.example.test?mode=test",
    "https://api.example.test/path with space",
    "https://api.example.test\\other",
    `https://api.example.test/${"a".repeat(2048)}`,
  ];

  for (const value of invalidValues) {
    await t.test(value.slice(0, 80), async () => {
      const clock = new FakeClock();
      const player = startPlayer("https://foo.github.io/raydio/", stationFixtures, {
        clock,
        configEntries: [value],
      });
      await settle();

      assert.equal(player.elements.status.textContent, "reconnecting");
      assert.deepEqual(apiFetches(player), []);
      assert.deepEqual(clock.pendingDelays(), [0]);
    });
  }
});
