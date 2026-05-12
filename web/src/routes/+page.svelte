<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import L from 'leaflet';
  import 'leaflet/dist/leaflet.css';

  type Vehicle = {
    id: string;
    route?: string;
    trip?: string;
    lat: number;
    lon: number;
    bearing?: number;
    speed?: number;
    ts: number;
    mode?: string;
  };
  type Agency = { code: string; name: string };
  type Track = {
    fromLat: number; fromLon: number;
    toLat: number;   toLon: number;
    fromTime: number; toTime: number;
    v: Vehicle;
  };

  // Lerp duration tracks the server's poll interval so each animation
  // segment runs out roughly when the next snapshot arrives.

  // Center per agency (rough — bay-area-wide for RG).
  const agencyCenters: Record<string, [number, number]> = {
    SF: [37.7749, -122.4194],
    BA: [37.7793, -122.275],
    CT: [37.55, -122.25],
    AC: [37.8, -122.27],
    SC: [37.35, -121.95],
    GG: [37.95, -122.5],
    SM: [37.55, -122.3],
    RG: [37.78, -122.3]
  };

  let mapEl = $state<HTMLDivElement | undefined>();
  let map: L.Map | undefined;
  const markers = new Map<string, L.CircleMarker>();
  const tracks  = new Map<string, Track>();

  let vehicles  = $state<Vehicle[]>([]);
  let connected = $state(false);
  let lastUpdate = $state<number | null>(null);
  let mapError = $state<string | null>(null);
  let agencies = $state<Agency[]>([]);
  let agencyCode = $state<string>('SF');
  let switching = $state(false);
  let pollIntervalSecs = $state<number>(30);
  let allowedIntervals = $state<number[]>([5, 15, 30, 60]);
  let intervalUpdating = $state(false);

  type RateLimitInfo = { at: number; message: string };
  let rateLimit = $state<RateLimitInfo | null>(null);
  let lastSeenRateLimitAt = $state<number>(0);
  let showRateLimitModal = $state(false);

  let es: EventSource | undefined;
  let rafId: number | undefined;

  const lerpMs = $derived(pollIntervalSecs * 1000);
  const requestsPerHour = $derived(Math.round(3600 / pollIntervalSecs));
  const RATE_LIMIT_PER_HOUR = 60;

  const routeCounts = $derived.by(() => {
    const counts = new Map<string, { count: number; mode: string }>();
    for (const v of vehicles) {
      const r = v.route || 'unknown';
      const e = counts.get(r);
      if (e) e.count += 1;
      else counts.set(r, { count: 1, mode: v.mode || 'Unknown' });
    }
    return [...counts.entries()].sort((a, b) => b[1].count - a[1].count);
  });

  const modeCounts = $derived.by(() => {
    const counts = new Map<string, number>();
    for (const v of vehicles) {
      const m = v.mode || 'Unknown';
      counts.set(m, (counts.get(m) ?? 0) + 1);
    }
    return [...counts.entries()].sort((a, b) => b[1] - a[1]);
  });

  function colorForRoute(route: string | undefined): string {
    if (!route) return '#94a3b8';
    let h = 0;
    for (let i = 0; i < route.length; i++) h = (h * 31 + route.charCodeAt(i)) >>> 0;
    return `hsl(${h % 360}, 75%, 55%)`;
  }

  function tooltipHtml(v: Vehicle): string {
    const route = v.route ?? 'unknown';
    const mode = v.mode ?? '';
    return `<div style="line-height:1.35">
      <strong>${route}</strong>
      <span style="color:#94a3b8;margin-left:6px">${mode}</span>
      <div style="color:#94a3b8;font-size:11px">id ${v.id}</div>
    </div>`;
  }

  function ensureMarker(v: Vehicle, lat: number, lon: number) {
    if (!map) return;
    let m = markers.get(v.id);
    if (!m) {
      m = L.circleMarker([lat, lon], {
        radius: 5,
        fillColor: colorForRoute(v.route),
        color: '#ffffff',
        weight: 1.5,
        fillOpacity: 0.95
      }).bindTooltip(tooltipHtml(v), { direction: 'top' });
      m.addTo(map);
      markers.set(v.id, m);
    } else {
      m.setTooltipContent(tooltipHtml(v));
    }
  }

  function applySnapshot(payload: { vehicles: Vehicle[]; rateLimited: RateLimitInfo | null }) {
    rateLimit = payload.rateLimited;
    if (payload.rateLimited && payload.rateLimited.at > lastSeenRateLimitAt) {
      lastSeenRateLimitAt = payload.rateLimited.at;
      showRateLimitModal = true;
    }
    applyUpdate(payload.vehicles ?? []);
  }

  function applyUpdate(next: Vehicle[]) {
    vehicles = next;
    lastUpdate = Date.now();
    if (!map) return;
    const now = Date.now();
    const seen = new Set<string>();
    for (const v of next) {
      seen.add(v.id);
      const t = tracks.get(v.id);
      if (!t) {
        tracks.set(v.id, {
          fromLat: v.lat, fromLon: v.lon,
          toLat: v.lat,   toLon: v.lon,
          fromTime: now,  toTime: now,
          v
        });
        ensureMarker(v, v.lat, v.lon);
      } else if (t.toLat !== v.lat || t.toLon !== v.lon) {
        // Snapshot current interpolated position → becomes new "from".
        const span = Math.max(1, t.toTime - t.fromTime);
        const p = Math.min(1, (now - t.fromTime) / span);
        t.fromLat = t.fromLat + (t.toLat - t.fromLat) * p;
        t.fromLon = t.fromLon + (t.toLon - t.fromLon) * p;
        t.fromTime = now;
        t.toLat = v.lat;
        t.toLon = v.lon;
        t.toTime = now + lerpMs;
        t.v = v;
        ensureMarker(v, t.fromLat, t.fromLon);
      } else {
        t.v = v;
        ensureMarker(v, t.fromLat, t.fromLon); // refresh tooltip in case mode/route updated
      }
    }
    for (const id of [...tracks.keys()]) {
      if (!seen.has(id)) {
        tracks.delete(id);
        markers.get(id)?.remove();
        markers.delete(id);
      }
    }
  }

  function tick() {
    const now = Date.now();
    for (const [id, t] of tracks) {
      const span = Math.max(1, t.toTime - t.fromTime);
      const p = Math.min(1, (now - t.fromTime) / span);
      const lat = t.fromLat + (t.toLat - t.fromLat) * p;
      const lon = t.fromLon + (t.toLon - t.fromLon) * p;
      markers.get(id)?.setLatLng([lat, lon]);
    }
    rafId = requestAnimationFrame(tick);
  }

  async function dismissRateLimitModalAndFallTo60s() {
    showRateLimitModal = false;
    await setPollInterval(60);
  }

  async function setPollInterval(secs: number) {
    if (secs === pollIntervalSecs || intervalUpdating) return;
    if (!allowedIntervals.includes(secs)) return;
    intervalUpdating = true;
    const prev = pollIntervalSecs;
    pollIntervalSecs = secs;
    try {
      const r = await fetch('/api/poll-interval', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ seconds: secs })
      });
      if (!r.ok) {
        pollIntervalSecs = prev;
        console.error('poll-interval rejected', await r.text());
      }
    } catch (e) {
      pollIntervalSecs = prev;
      console.error('poll-interval failed', e);
    } finally {
      intervalUpdating = false;
    }
  }

  async function switchAgency(code: string) {
    if (code === agencyCode || switching) return;
    switching = true;
    agencyCode = code;
    // Clear client state immediately so we don't lerp old positions into new agency.
    for (const m of markers.values()) m.remove();
    markers.clear();
    tracks.clear();
    vehicles = [];
    const c = agencyCenters[code] ?? [37.78, -122.3];
    map?.setView(c, code === 'RG' ? 10 : 11);
    try {
      await fetch('/api/agency', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ agency: code })
      });
    } catch (e) {
      console.error('agency switch failed', e);
    } finally {
      switching = false;
    }
  }

  onMount(async () => {
    if (!mapEl) {
      mapError = 'mapEl was undefined at onMount';
      return;
    }
    try {
      map = L.map(mapEl, {
        center: agencyCenters.SF,
        zoom: 12,
        preferCanvas: true,
        zoomControl: true
      });
      L.tileLayer('https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', {
        subdomains: 'abc',
        maxZoom: 19,
        attribution: '© OpenStreetMap contributors'
      }).addTo(map);
      requestAnimationFrame(() => map?.invalidateSize());
    } catch (err) {
      mapError = `init threw: ${err instanceof Error ? err.message : String(err)}`;
      return;
    }

    try {
      const r = await fetch('/api/agency');
      if (r.ok) {
        const data = await r.json();
        agencyCode = data.agency || 'SF';
        agencies = data.available || [];
        const c = agencyCenters[agencyCode] ?? [37.78, -122.3];
        map.setView(c, agencyCode === 'RG' ? 10 : 11);
      }
    } catch {}

    try {
      const r = await fetch('/api/poll-interval');
      if (r.ok) {
        const data = await r.json();
        if (typeof data.seconds === 'number') pollIntervalSecs = data.seconds;
        if (Array.isArray(data.allowed)) allowedIntervals = data.allowed;
      }
    } catch {}

    try {
      const r = await fetch('/api/vehicles');
      if (r.ok) applySnapshot(await r.json());
    } catch (e) {
      console.error('initial load failed', e);
    }

    es = new EventSource('/api/stream');
    es.onopen = () => (connected = true);
    es.onerror = () => (connected = false);
    es.onmessage = (ev) => {
      try {
        applySnapshot(JSON.parse(ev.data));
      } catch (e) {
        console.error('parse error', e);
      }
    };

    rafId = requestAnimationFrame(tick);
  });

  onDestroy(() => {
    if (rafId) cancelAnimationFrame(rafId);
    es?.close();
    map?.remove();
  });
</script>

<div class="flex h-screen w-screen">
  <aside class="w-72 shrink-0 border-r border-slate-800 bg-slate-900/80 p-4 overflow-y-auto">
    <h1 class="text-lg font-semibold tracking-tight">Transit Ops Center</h1>
    <p class="mt-1 text-xs text-slate-400">Live Bay Area vehicle positions · GTFS-RT via 511.org</p>

    <label class="mt-4 block text-[11px] font-semibold uppercase tracking-wide text-slate-400">Agency</label>
    <select
      class="mt-1 w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm text-slate-100 focus:outline-none focus:ring-1 focus:ring-emerald-500"
      value={agencyCode}
      disabled={switching || agencies.length === 0}
      onchange={(e) => switchAgency((e.currentTarget as HTMLSelectElement).value)}
    >
      {#if agencies.length === 0}
        <option value="SF">SF Muni</option>
      {/if}
      {#each agencies as a}
        <option value={a.code}>{a.name}</option>
      {/each}
    </select>

    <label class="mt-4 block text-[11px] font-semibold uppercase tracking-wide text-slate-400">
      Refresh rate
    </label>
    <div class="mt-1 grid grid-cols-4 gap-1">
      {#each allowedIntervals as secs}
        <button
          type="button"
          disabled={intervalUpdating}
          onclick={() => setPollInterval(secs)}
          class="rounded-md border px-1.5 py-1 text-xs font-medium transition
            {secs === pollIntervalSecs
              ? 'border-emerald-500 bg-emerald-500/15 text-emerald-300'
              : 'border-slate-700 bg-slate-800 text-slate-300 hover:bg-slate-700'}"
        >
          {secs}s
        </button>
      {/each}
    </div>
    <div class="mt-1 text-[10px] {requestsPerHour > RATE_LIMIT_PER_HOUR ? 'text-amber-400' : 'text-slate-500'}">
      {requestsPerHour} req/hr
      {#if requestsPerHour > RATE_LIMIT_PER_HOUR}
        · {Math.round(requestsPerHour / RATE_LIMIT_PER_HOUR)}× over 511's documented limit
      {:else}
        · within 511's documented 60 req/hr limit
      {/if}
    </div>

    <div class="mt-4 flex items-center gap-2 text-xs">
      <span class="inline-flex h-2 w-2 rounded-full {connected ? 'bg-emerald-400' : 'bg-rose-500'}"></span>
      <span class="text-slate-300">{connected ? 'streaming' : 'disconnected'}</span>
      {#if lastUpdate}
        <span class="ml-auto text-slate-500">{new Date(lastUpdate).toLocaleTimeString()}</span>
      {/if}
    </div>

    {#if mapError}
      <div class="mt-3 rounded-md border border-rose-900/60 bg-rose-950/40 p-2 text-[11px] font-mono leading-relaxed text-rose-300 break-words">
        {mapError}
      </div>
    {/if}

    <div class="mt-4 rounded-md bg-slate-800/60 p-3">
      <div class="text-2xl font-semibold">{vehicles.length}</div>
      <div class="text-xs uppercase tracking-wide text-slate-400">vehicles tracked</div>
    </div>

    {#if modeCounts.length > 0}
      <h2 class="mt-6 text-xs font-semibold uppercase tracking-wide text-slate-400">Modes</h2>
      <ul class="mt-2 space-y-1 text-sm">
        {#each modeCounts as [mode, count]}
          <li class="flex items-center gap-2 text-slate-200">
            <span class="text-slate-300">{mode}</span>
            <span class="ml-auto text-slate-500">{count}</span>
          </li>
        {/each}
      </ul>
    {/if}

    <h2 class="mt-6 text-xs font-semibold uppercase tracking-wide text-slate-400">Routes</h2>
    <ul class="mt-2 space-y-1 text-sm">
      {#each routeCounts as [route, info]}
        <li
          class="flex items-center gap-2 cursor-help"
          title={`${info.mode} · ${info.count} vehicle${info.count === 1 ? '' : 's'}`}
        >
          <span class="h-3 w-3 rounded-full" style="background: {colorForRoute(route)}"></span>
          <span class="text-slate-200">{route}</span>
          <span class="ml-auto text-slate-500">{info.count}</span>
        </li>
      {/each}
    </ul>
  </aside>

  <main class="relative flex-1">
    <div bind:this={mapEl} class="absolute inset-0" style="width: 100%; height: 100%; background: #0f172a;"></div>
  </main>
</div>

{#if showRateLimitModal && rateLimit}
  <div
    class="fixed inset-0 z-[1000] flex items-center justify-center bg-black/70 backdrop-blur-sm"
    role="dialog"
    aria-modal="true"
    aria-labelledby="rl-title"
  >
    <div class="mx-4 w-full max-w-md rounded-lg border border-rose-700 bg-slate-900 p-5 shadow-2xl">
      <div class="flex items-start gap-3">
        <div class="mt-0.5 flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-rose-500/20 text-rose-300">
          <span class="text-lg font-bold">!</span>
        </div>
        <div class="flex-1">
          <h3 id="rl-title" class="text-base font-semibold text-rose-200">Rate limit hit</h3>
          <p class="mt-2 text-sm leading-relaxed text-slate-300">{rateLimit.message}</p>
          <p class="mt-3 text-xs text-slate-400">
            Clicking OK will switch the poll interval to <strong class="text-slate-200">60 seconds</strong>
            (60 req/hr — compliant with 511's documented limit).
          </p>
        </div>
      </div>
      <div class="mt-5 flex justify-end">
        <button
          type="button"
          onclick={dismissRateLimitModalAndFallTo60s}
          class="rounded-md bg-rose-600 px-4 py-2 text-sm font-medium text-white hover:bg-rose-500 focus:outline-none focus:ring-2 focus:ring-rose-400"
        >
          OK
        </button>
      </div>
    </div>
  </div>
{/if}

<style>
  :global(.leaflet-control-attribution) {
    background: rgba(15, 23, 42, 0.85) !important;
    color: #94a3b8 !important;
  }
  :global(.leaflet-control-attribution a) {
    color: #cbd5e1 !important;
  }
  :global(.leaflet-tooltip) {
    background: rgba(15, 23, 42, 0.95);
    border: 1px solid #334155;
    color: #e2e8f0;
    box-shadow: 0 4px 12px rgba(0, 0, 0, 0.45);
    padding: 6px 10px;
  }
  :global(.leaflet-tooltip-top:before) {
    border-top-color: #334155;
  }
</style>
