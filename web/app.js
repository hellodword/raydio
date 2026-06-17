const audio = document.getElementById("audio");
const stationSelect = document.getElementById("station");
const playButton = document.getElementById("play");
const pauseButton = document.getElementById("pause");
const volume = document.getElementById("volume");
const title = document.getElementById("title");
const artist = document.getElementById("artist");
const statusText = document.getElementById("status");
const cover = document.getElementById("cover");
const coverFallback = document.getElementById("coverFallback");

let currentNow = null;
let stations = [];
let currentStation = null;
let events = null;

audio.volume = Number(volume.value);

playButton.addEventListener("click", async () => {
  if (!currentStation) {
    statusText.textContent = "no station";
    return;
  }
  try {
    statusText.textContent = "playing";
    await audio.play();
  } catch (err) {
    statusText.textContent = "play blocked";
  }
});

pauseButton.addEventListener("click", () => {
  audio.pause();
  statusText.textContent = "paused";
});

volume.addEventListener("input", () => {
  audio.volume = Number(volume.value);
});

stationSelect.addEventListener("change", () => {
  const station = stations.find((item) => item.alias === stationSelect.value || item.uuid === stationSelect.value);
  if (station) setStation(station);
});

function stationPath() {
  if (!currentStation) return "";
  return `/radio/${encodeURIComponent(currentStation.alias || currentStation.uuid)}`;
}

async function loadStations() {
  const res = await fetch("/api/stations", { cache: "no-store" });
  if (!res.ok) throw new Error(`stations ${res.status}`);
  const body = await res.json();
  stations = Array.isArray(body.stations) ? body.stations : [];
  stationSelect.replaceChildren(
    ...stations.map((station) => {
      const option = document.createElement("option");
      option.value = station.alias || station.uuid;
      option.textContent = station.alias || station.uuid;
      return option;
    }),
  );
  if (stations.length === 0) {
    statusText.textContent = "no station";
    return;
  }
  setStation(stations[0]);
}

function setStation(station) {
  currentStation = station;
  stationSelect.value = station.alias || station.uuid;
  if (events) {
    events.close();
    events = null;
  }
  audio.pause();
  audio.src = stationPath();
  resetTrackState();
  statusText.textContent = "ready";
  loadNow().catch(() => {
    statusText.textContent = "offline";
  });
  connectEvents();
}

async function loadNow() {
  const res = await fetch(`${stationPath()}/api/now`, { cache: "no-store" });
  if (!res.ok) throw new Error(`now ${res.status}`);
  renderNow(await res.json());
}

function resetTrackState() {
  currentNow = null;
  title.textContent = "No track";
  artist.textContent = "Unknown artist";
  cover.removeAttribute("src");
  cover.style.display = "none";
  coverFallback.style.display = "grid";
}

function renderNow(now) {
  currentNow = now;

  if (now.isSilence || !now.track) {
    title.textContent = "Silence";
    artist.textContent = "No active track";
    cover.style.display = "none";
    coverFallback.style.display = "grid";
    statusText.textContent = audio.paused ? "ready" : "playing";
    return;
  }

  title.textContent = now.track.title || "Unknown title";
  artist.textContent = now.track.artist || "Unknown artist";
  statusText.textContent = audio.paused ? "ready" : "playing";

  if (now.track.coverUrl) {
    cover.src = `${now.track.coverUrl}?v=${encodeURIComponent(now.track.id)}`;
    cover.style.display = "block";
    coverFallback.style.display = "none";
  } else {
    cover.removeAttribute("src");
    cover.style.display = "none";
    coverFallback.style.display = "grid";
  }
}

function connectEvents() {
  events = new EventSource(`${stationPath()}/api/events`);
  events.addEventListener("now", (ev) => renderNow(JSON.parse(ev.data)));
  events.addEventListener("open", () => {
    statusText.textContent = audio.paused ? "ready" : "playing";
  });
  events.addEventListener("error", () => {
    statusText.textContent = "reconnecting";
  });
}

loadStations().catch(() => {
  statusText.textContent = "offline";
});
