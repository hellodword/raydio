const audio = document.getElementById("audio");
const playButton = document.getElementById("play");
const pauseButton = document.getElementById("pause");
const volume = document.getElementById("volume");
const title = document.getElementById("title");
const artist = document.getElementById("artist");
const statusText = document.getElementById("status");
const lyricText = document.getElementById("lyric");
const cover = document.getElementById("cover");
const coverFallback = document.getElementById("coverFallback");

let currentNow = null;
let lyrics = [];
let serverOffsetMs = 0;

audio.volume = Number(volume.value);

playButton.addEventListener("click", async () => {
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

async function loadNow() {
  const res = await fetch("/api/now", { cache: "no-store" });
  if (!res.ok) throw new Error(`now ${res.status}`);
  renderNow(await res.json());
}

function renderNow(now) {
  currentNow = now;
  serverOffsetMs = now.serverTimeMs - Date.now();

  if (now.isSilence || !now.track) {
    title.textContent = "Silence";
    artist.textContent = "No active track";
    lyricText.textContent = "";
    cover.style.display = "none";
    coverFallback.style.display = "grid";
    statusText.textContent = audio.paused ? "ready" : "playing";
    lyrics = [];
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

  if (now.track.lyricsUrl) {
    fetch(now.track.lyricsUrl, { cache: "no-store" })
      .then((res) => (res.ok ? res.text() : ""))
      .then((text) => {
        lyrics = parseLRC(text);
        updateLyric();
      })
      .catch(() => {
        lyrics = [];
        lyricText.textContent = "";
      });
  } else {
    lyrics = [];
    lyricText.textContent = "";
  }
}

function parseLRC(text) {
  const out = [];
  for (const line of text.split(/\r?\n/)) {
    const match = line.match(/^\[(\d{1,2}):(\d{2})(?:\.(\d{1,3}))?\](.*)$/);
    if (!match) continue;
    const min = Number(match[1]);
    const sec = Number(match[2]);
    const frac = Number((match[3] || "0").padEnd(3, "0"));
    out.push({ at: min * 60000 + sec * 1000 + frac, text: match[4].trim() });
  }
  return out.sort((a, b) => a.at - b.at);
}

function updateLyric() {
  if (!currentNow || lyrics.length === 0) return;
  const serverNow = Date.now() + serverOffsetMs;
  const elapsed = serverNow - currentNow.startedAtMs;
  let current = "";
  for (const row of lyrics) {
    if (row.at <= elapsed) current = row.text;
    else break;
  }
  lyricText.textContent = current;
}

function connectEvents() {
  const events = new EventSource("/api/events");
  events.addEventListener("now", (ev) => renderNow(JSON.parse(ev.data)));
  events.addEventListener("open", () => {
    statusText.textContent = audio.paused ? "ready" : "playing";
  });
  events.addEventListener("error", () => {
    statusText.textContent = "reconnecting";
  });
}

setInterval(updateLyric, 500);
loadNow().catch(() => {
  statusText.textContent = "offline";
});
connectEvents();
