const audio = document.getElementById("audio");
const stationSelect = document.getElementById("station");
const playButton = document.getElementById("play");
const title = document.getElementById("title");
const artist = document.getElementById("artist");
const statusText = document.getElementById("status");
const cover = document.getElementById("cover");
const coverFallback = document.getElementById("coverFallback");
const streamUrl = document.getElementById("streamUrl");
const streamCommand = document.getElementById("streamCommand");
const copyStatus = document.getElementById("copyStatus");

const MAX_BASE_URL_LENGTH = 2048;
const REQUEST_TIMEOUT_MS = 10000;
const RECOVERY_RETRY_DELAYS_MS = [2000, 5000, 10000, 30000];

let apiBaseURL = "";
let currentNow = null;
let stations = [];
let currentStation = null;
let events = null;
let desiredPlaying = false;
let playbackBlocked = false;
let recoveryMode = null;
let recoveryTimer = null;
let recoveryRunning = false;
let recoveryFailures = 0;
let configRequestSequence = 0;

function normalizeAPIBaseURL(value) {
  if (
    typeof value !== "string" ||
    value.length > MAX_BASE_URL_LENGTH ||
    value !== value.trim() ||
    /[\s\\"'<>]/u.test(value)
  ) {
    throw new Error("invalid API base URL");
  }
  if (!value) return "";
  if (!/^https:\/\/[^/]/iu.test(value)) throw new Error("invalid API base URL");
  try {
    const url = new URL(value);
    if (
      url.protocol !== "https:" ||
      !url.hostname ||
      url.username ||
      url.password ||
      url.search ||
      url.hash
    ) {
      throw new Error("invalid API base URL");
    }
    return url.toString().replace(/\/$/, "");
  } catch (_err) {
    throw new Error("invalid API base URL");
  }
}

function parseRuntimeConfig(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("invalid runtime config");
  }
  const keys = Object.keys(value);
  if (keys.length !== 1 || keys[0] !== "apiBaseUrl") {
    throw new Error("invalid runtime config");
  }
  return normalizeAPIBaseURL(value.apiBaseUrl);
}

function apiURL(path, baseURL = apiBaseURL) {
  if (!baseURL) return path;
  return new URL(String(path).replace(/^\/+/, ""), `${baseURL}/`).toString();
}

function runtimeConfigURL() {
  const url = new URL("./config.json", document.baseURI || window.location.href);
  configRequestSequence += 1;
  url.searchParams.set("request", `${Date.now()}-${configRequestSequence}`);
  return url.toString();
}

async function fetchWithTimeout(url, options = {}) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS);
  try {
    return await fetch(url, { ...options, signal: controller.signal });
  } finally {
    clearTimeout(timeout);
  }
}

async function fetchRuntimeConfig() {
  const res = await fetchWithTimeout(runtimeConfigURL(), { cache: "no-store" });
  if (!res.ok) throw new Error(`config ${res.status}`);
  return parseRuntimeConfig(await res.json());
}

playButton.addEventListener("click", async () => {
  if (desiredPlaying) {
    stopAudio("stopped");
    return;
  }
  if (!currentStation) {
    statusText.textContent = "no station";
    return;
  }
  desiredPlaying = true;
  playbackBlocked = false;
  updatePlayButton();
  await playAudio();
});

bindCopyBox(streamUrl);
bindCopyBox(streamCommand);

audio.addEventListener("play", () => {
  desiredPlaying = true;
  playbackBlocked = false;
  updatePlayButton();
  syncRuntimeStatus();
});
audio.addEventListener("pause", updatePlayButton);
audio.addEventListener("ended", () => {
  updatePlayButton();
  if (desiredPlaying) requestRecovery("active");
});
audio.addEventListener("error", () => {
  if (desiredPlaying) requestRecovery("active");
});

stationSelect.addEventListener("change", () => {
  const station = findStation(stationSelect.value);
  if (station) setStation(station);
});

window.addEventListener("online", retryRecoveryNow);

function stationIdentifier(station) {
  if (typeof station?.alias === "string" && station.alias) return station.alias;
  if (typeof station?.uuid === "string" && station.uuid) return station.uuid;
  return "";
}

function findStation(identifier, candidates = stations) {
  if (!identifier) return null;
  return (
    candidates.find((item) => item.alias === identifier || item.uuid === identifier) || null
  );
}

function preferredStation(candidates) {
  const requested = new URL(window.location.href).searchParams.get("raydio");
  const identifiers = [requested, currentStation?.alias, currentStation?.uuid];
  for (const identifier of identifiers) {
    const station = findStation(identifier, candidates);
    if (station) return station;
  }
  return findStation("all", candidates);
}

function syncStationQuery(station) {
  const identifier = stationIdentifier(station);
  if (!identifier) return;

  const url = new URL(window.location.href);
  const currentValues = url.searchParams.getAll("raydio");
  if (
    currentValues.length === 1 &&
    (currentValues[0] === station.alias || currentValues[0] === station.uuid)
  ) {
    return;
  }

  url.searchParams.set("raydio", identifier);
  window.history.replaceState(null, "", `${url.pathname}${url.search}${url.hash}`);
}

function stationPath() {
  if (!currentStation) return "";
  return `/radio/${encodeURIComponent(currentStation.alias || currentStation.uuid)}`;
}

function stationURL() {
  if (!currentStation) return "";
  return new URL(apiURL(stationPath()), window.location.href).toString();
}

function shellQuote(value) {
  return `'${String(value).replaceAll("'", "'\\''")}'`;
}

function artistURL(handle) {
  const cleanHandle = String(handle).trim().replace(/^@+/, "");
  return cleanHandle ? `https://suno.com/@${encodeURIComponent(cleanHandle)}` : "";
}

function setArtist(handle, linkable = true) {
  const value = handle || "Unknown artist";
  artist.textContent = value;
  const url = linkable ? artistURL(value) : "";
  if (url) {
    artist.href = url;
    artist.removeAttribute("aria-disabled");
  } else {
    artist.removeAttribute("href");
    artist.setAttribute("aria-disabled", "true");
  }
}

function runtimeStatus() {
  if (recoveryMode) return "reconnecting";
  if (!currentStation) return "no station";
  if (playbackBlocked) return "play blocked";
  return desiredPlaying && !audio.paused ? "playing" : "ready";
}

function syncRuntimeStatus() {
  statusText.textContent = runtimeStatus();
}

function updatePlayButton() {
  playButton.textContent = desiredPlaying ? "停止" : "播放";
  playButton.setAttribute("aria-pressed", String(desiredPlaying));
}

async function playAudio() {
  statusText.textContent = "playing";
  try {
    await audio.play();
    updatePlayButton();
    return true;
  } catch (_err) {
    desiredPlaying = false;
    playbackBlocked = true;
    statusText.textContent = "play blocked";
    updatePlayButton();
    return false;
  }
}

function stopAudio(status) {
  desiredPlaying = false;
  playbackBlocked = false;
  audio.pause();
  statusText.textContent = status;
  updatePlayButton();
}

function updateCommand() {
  const url = stationURL();
  const command = url ? `curl -sN ${shellQuote(url)} | ffplay -hide_banner -nodisp -f mp3 -` : "";
  streamUrl.textContent = url;
  streamCommand.textContent = command;
  copyStatus.textContent = "";
}

function bindCopyBox(element) {
  element.addEventListener("click", () => copyBoxText(element));
  element.addEventListener("keydown", (ev) => {
    if (ev.key !== "Enter" && ev.key !== " ") return;
    ev.preventDefault();
    copyBoxText(element);
  });
}

async function copyBoxText(element) {
  const text = element.textContent.trim();
  if (!text) return;
  selectElementText(element);
  try {
    await copyText(text);
    copyStatus.textContent = "copied";
  } catch (_err) {
    copyStatus.textContent = "copy failed";
  } finally {
    selectElementText(element);
  }
}

function selectElementText(element) {
  const selection = window.getSelection();
  const range = document.createRange();
  range.selectNodeContents(element);
  selection.removeAllRanges();
  selection.addRange(range);
}

async function copyText(text) {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return;
    } catch (_err) {
      fallbackCopyText(text);
      return;
    }
  }
  fallbackCopyText(text);
}

function fallbackCopyText(text) {
  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.setAttribute("readonly", "");
  textarea.style.position = "fixed";
  textarea.style.left = "-9999px";
  document.body.appendChild(textarea);
  textarea.select();
  const copied = document.execCommand("copy");
  textarea.remove();
  if (!copied) throw new Error("copy failed");
}

async function fetchStations(baseURL) {
  const res = await fetchWithTimeout(apiURL("/api/stations", baseURL), { cache: "no-store" });
  if (!res.ok) throw new Error(`stations ${res.status}`);
  const body = await res.json();
  const nextStations = (Array.isArray(body.stations) ? body.stations : []).filter(
    (station) => station && typeof station === "object" && stationIdentifier(station),
  );
  const aggregateIndex = nextStations.findIndex((station) => station.alias === "all");
  if (aggregateIndex > 0) {
    nextStations.unshift(nextStations.splice(aggregateIndex, 1)[0]);
  }
  return nextStations;
}

function renderStations(nextStations) {
  stationSelect.replaceChildren(
    ...nextStations.map((station) => {
      const option = document.createElement("option");
      option.value = stationIdentifier(station);
      option.textContent = stationIdentifier(station);
      return option;
    }),
  );
}

async function activateBaseURL(nextBaseURL, preservePlayback) {
  const nextStations = await fetchStations(nextBaseURL);
  const station = preferredStation(nextStations);
  const shouldResume = preservePlayback && desiredPlaying;

  apiBaseURL = nextBaseURL;
  stations = nextStations;
  renderStations(stations);

  if (!station) {
    if (events) events.close();
    events = null;
    currentStation = null;
    desiredPlaying = false;
    playbackBlocked = false;
    stationSelect.value = "";
    audio.pause();
    audio.removeAttribute("src");
    resetTrackState();
    statusText.textContent = "no station";
    updatePlayButton();
    return;
  }

  await setStation(station, { preservePlayback, resumePlayback: shouldResume });
}

async function setStation(station, options = {}) {
  const preservePlayback = Boolean(options.preservePlayback);
  const resumePlayback = preservePlayback && Boolean(options.resumePlayback) && desiredPlaying;
  if (!preservePlayback) desiredPlaying = false;
  playbackBlocked = false;

  currentStation = station;
  stationSelect.value = stationIdentifier(station);
  syncStationQuery(station);
  if (events) events.close();
  events = null;
  audio.pause();
  audio.src = apiURL(stationPath());
  resetTrackState();
  syncRuntimeStatus();
  refreshNow();
  connectEvents();

  if (resumePlayback) await playAudio();
  else updatePlayButton();
}

async function refreshNow() {
  try {
    const res = await fetchWithTimeout(apiURL(`${stationPath()}/api/now`), {
      cache: "no-store",
    });
    if (!res.ok) throw new Error(`now ${res.status}`);
    renderNow(await res.json());
    finishPassiveRecovery();
  } catch (_err) {
    requestRecovery("passive");
  }
}

function resetTrackState() {
  currentNow = null;
  title.textContent = "No track";
  setArtist("Unknown artist", false);
  cover.removeAttribute("src");
  cover.style.display = "none";
  coverFallback.style.display = "grid";
  updateCommand();
}

function renderNow(now) {
  currentNow = now;

  if (now.isSilence || !now.track) {
    title.textContent = "Silence";
    setArtist("No active track", false);
    cover.style.display = "none";
    coverFallback.style.display = "grid";
    syncRuntimeStatus();
    return;
  }

  title.textContent = now.track.title || "Unknown title";
  setArtist(now.track.artist || "Unknown artist", Boolean(now.track.artist));
  syncRuntimeStatus();

  if (now.track.coverUrl) {
    cover.src = `${apiURL(now.track.coverUrl)}?v=${encodeURIComponent(now.track.id)}`;
    cover.style.display = "block";
    coverFallback.style.display = "none";
  } else {
    cover.removeAttribute("src");
    cover.style.display = "none";
    coverFallback.style.display = "grid";
  }
}

function connectEvents() {
  const source = new EventSource(apiURL(`${stationPath()}/api/events`));
  events = source;
  source.addEventListener("now", (ev) => {
    if (events !== source) return;
    try {
      renderNow(JSON.parse(ev.data));
      finishPassiveRecovery();
    } catch (_err) {
      requestRecovery("passive");
    }
  });
  source.addEventListener("open", () => {
    if (events !== source) return;
    finishPassiveRecovery();
    syncRuntimeStatus();
  });
  source.addEventListener("error", () => {
    if (events !== source) return;
    requestRecovery("passive");
  });
}

function requestRecovery(mode) {
  if (mode === "active" || recoveryMode === null) recoveryMode = mode;
  statusText.textContent = "reconnecting";
  if (!recoveryRunning && recoveryTimer === null) scheduleRecovery(0);
}

function scheduleRecovery(delay) {
  if (!recoveryMode || recoveryTimer !== null) return;
  recoveryTimer = setTimeout(runRecovery, delay);
}

function nextRecoveryDelay() {
  const index = Math.min(recoveryFailures, RECOVERY_RETRY_DELAYS_MS.length - 1);
  recoveryFailures += 1;
  return RECOVERY_RETRY_DELAYS_MS[index];
}

async function runRecovery() {
  recoveryTimer = null;
  if (!recoveryMode || recoveryRunning) return;
  recoveryRunning = true;
  let recovered = false;
  try {
    const nextBaseURL = await fetchRuntimeConfig();
    if (!recoveryMode) return;
    if (nextBaseURL !== apiBaseURL || recoveryMode === "active") {
      await activateBaseURL(nextBaseURL, true);
      recovered = true;
    }
  } catch (_err) {
    recovered = false;
  } finally {
    recoveryRunning = false;
    if (recovered) {
      stopRecovery();
      syncRuntimeStatus();
    } else if (recoveryMode && recoveryTimer === null) {
      scheduleRecovery(nextRecoveryDelay());
    }
  }
}

function finishPassiveRecovery() {
  if (recoveryMode !== "passive") return;
  stopRecovery();
  syncRuntimeStatus();
}

function stopRecovery() {
  recoveryMode = null;
  recoveryFailures = 0;
  if (recoveryTimer !== null) clearTimeout(recoveryTimer);
  recoveryTimer = null;
}

function retryRecoveryNow() {
  if (!recoveryMode || recoveryRunning) return;
  if (recoveryTimer !== null) clearTimeout(recoveryTimer);
  recoveryTimer = null;
  scheduleRecovery(0);
}

async function startPlayer() {
  updateCommand();
  updatePlayButton();
  try {
    const initialBaseURL = await fetchRuntimeConfig();
    await activateBaseURL(initialBaseURL, false);
  } catch (_err) {
    requestRecovery("active");
  }
}

startPlayer();
