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
const apiBaseURL = configuredAPIBaseURL();

let currentNow = null;
let stations = [];
let currentStation = null;
let events = null;

function configuredAPIBaseURL() {
  const value = globalThis.RAYDIO_CONFIG?.apiBaseUrl;
  if (typeof value !== "string" || !value) return "";
  try {
    const url = new URL(value);
    if (
      url.protocol !== "https:" ||
      url.username ||
      url.password ||
      url.search ||
      url.hash
    ) {
      return "";
    }
    return url.toString().replace(/\/$/, "");
  } catch (_err) {
    return "";
  }
}

function apiURL(path) {
  if (!apiBaseURL) return path;
  return new URL(String(path).replace(/^\/+/, ""), `${apiBaseURL}/`).toString();
}

playButton.addEventListener("click", async () => {
  if (!currentStation) {
    statusText.textContent = "no station";
    return;
  }
  if (!audio.paused) {
    stopAudio("stopped");
    return;
  }
  try {
    statusText.textContent = "playing";
    await audio.play();
    updatePlayButton();
  } catch (err) {
    statusText.textContent = "play blocked";
    updatePlayButton();
  }
});

bindCopyBox(streamUrl);
bindCopyBox(streamCommand);

audio.addEventListener("play", updatePlayButton);
audio.addEventListener("pause", updatePlayButton);
audio.addEventListener("ended", updatePlayButton);

stationSelect.addEventListener("change", () => {
  const station = findStation(stationSelect.value);
  if (station) setStation(station);
});

function stationIdentifier(station) {
  if (typeof station?.alias === "string" && station.alias) return station.alias;
  if (typeof station?.uuid === "string" && station.uuid) return station.uuid;
  return "";
}

function findStation(identifier) {
  if (!identifier) return null;
  return stations.find((item) => item.alias === identifier || item.uuid === identifier) || null;
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

function updatePlayButton() {
  playButton.textContent = audio.paused ? "播放" : "停止";
  playButton.setAttribute("aria-pressed", String(!audio.paused));
}

function stopAudio(status) {
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
  } catch (err) {
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
    } catch (err) {
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

async function loadStations() {
  const res = await fetch(apiURL("/api/stations"), { cache: "no-store" });
  if (!res.ok) throw new Error(`stations ${res.status}`);
  const body = await res.json();
  stations = (Array.isArray(body.stations) ? body.stations : []).filter(
    (station) => station && typeof station === "object" && stationIdentifier(station),
  );
  const aggregateIndex = stations.findIndex((station) => station.alias === "all");
  if (aggregateIndex > 0) {
    stations.unshift(stations.splice(aggregateIndex, 1)[0]);
  }
  stationSelect.replaceChildren(
    ...stations.map((station) => {
      const option = document.createElement("option");
      option.value = stationIdentifier(station);
      option.textContent = stationIdentifier(station);
      return option;
    }),
  );
  if (stations.length === 0) {
    statusText.textContent = "no station";
    return;
  }
  const requestedStation = new URL(window.location.href).searchParams.get("raydio");
  const station = findStation(requestedStation) || findStation("all");
  if (!station) {
    statusText.textContent = "no station";
    return;
  }
  setStation(station);
}

function setStation(station) {
  currentStation = station;
  stationSelect.value = stationIdentifier(station);
  syncStationQuery(station);
  if (events) {
    events.close();
    events = null;
  }
  stopAudio("ready");
  audio.src = apiURL(stationPath());
  resetTrackState();
  statusText.textContent = "ready";
  loadNow().catch(() => {
    statusText.textContent = "offline";
  });
  connectEvents();
}

async function loadNow() {
  const res = await fetch(apiURL(`${stationPath()}/api/now`), { cache: "no-store" });
  if (!res.ok) throw new Error(`now ${res.status}`);
  renderNow(await res.json());
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
    statusText.textContent = audio.paused ? "ready" : "playing";
    return;
  }

  title.textContent = now.track.title || "Unknown title";
  setArtist(now.track.artist || "Unknown artist", Boolean(now.track.artist));
  statusText.textContent = audio.paused ? "ready" : "playing";

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
  events = new EventSource(apiURL(`${stationPath()}/api/events`));
  events.addEventListener("now", (ev) => renderNow(JSON.parse(ev.data)));
  events.addEventListener("open", () => {
    statusText.textContent = audio.paused ? "ready" : "playing";
  });
  events.addEventListener("error", () => {
    statusText.textContent = "reconnecting";
  });
}

updateCommand();
updatePlayButton();

loadStations().catch(() => {
  statusText.textContent = "offline";
  updatePlayButton();
});
