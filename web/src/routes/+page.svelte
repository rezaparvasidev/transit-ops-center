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
  };

  let mapEl = $state<HTMLDivElement | undefined>();
  let map: L.Map | undefined;
  const markers = new Map<string, L.CircleMarker>();
  let vehicles = $state<Vehicle[]>([]);
  let connected = $state(false);
  let lastUpdate = $state<number | null>(null);
  let mapStatus = $state<string>('initializing...');
  let mapError = $state<string | null>(null);
  let es: EventSource | undefined;

  const routeCounts = $derived.by(() => {
    const counts: Record<string, number> = {};
    for (const v of vehicles) {
      const r = v.route || 'unknown';
      counts[r] = (counts[r] ?? 0) + 1;
    }
    return Object.entries(counts).sort((a, b) => b[1] - a[1]);
  });

  function colorForRoute(route: string | undefined): string {
    if (!route) return '#94a3b8';
    let h = 0;
    for (let i = 0; i < route.length; i++) h = (h * 31 + route.charCodeAt(i)) >>> 0;
    return `hsl(${h % 360}, 75%, 55%)`;
  }

  function upsertMarker(v: Vehicle) {
    if (!map) return;
    const existing = markers.get(v.id);
    if (existing) {
      existing.setLatLng([v.lat, v.lon]);
      return;
    }
    const m = L.circleMarker([v.lat, v.lon], {
      radius: 5,
      fillColor: colorForRoute(v.route),
      color: '#ffffff',
      weight: 1.5,
      fillOpacity: 0.95
    }).bindTooltip(`${v.route ?? 'unknown'} • ${v.id}`, { direction: 'top' });
    m.addTo(map);
    markers.set(v.id, m);
  }

  function applyUpdate(next: Vehicle[]) {
    vehicles = next;
    lastUpdate = Date.now();
    if (!map) return;
    const seen = new Set<string>();
    for (const v of next) {
      seen.add(v.id);
      upsertMarker(v);
    }
    for (const [id, m] of markers) {
      if (!seen.has(id)) {
        m.remove();
        markers.delete(id);
      }
    }
  }

  onMount(async () => {
    if (!mapEl) {
      mapError = 'mapEl was undefined at onMount';
      return;
    }
    const r = mapEl.getBoundingClientRect();
    mapStatus = `container ${Math.round(r.width)}x${Math.round(r.height)}`;

    try {
      map = L.map(mapEl, {
        center: [37.7749, -122.4194],
        zoom: 12,
        preferCanvas: true,
        zoomControl: true
      });
      L.tileLayer('https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}.png', {
        subdomains: 'abcd',
        maxZoom: 19,
        attribution: '© OpenStreetMap contributors © CARTO'
      }).addTo(map);
      requestAnimationFrame(() => map?.invalidateSize());
      mapStatus = 'loaded';
    } catch (err) {
      mapError = `init threw: ${err instanceof Error ? err.message : String(err)}`;
      return;
    }

    try {
      const r2 = await fetch('/api/vehicles');
      if (r2.ok) applyUpdate(await r2.json());
    } catch (e) {
      console.error('initial load failed', e);
    }

    es = new EventSource('/api/stream');
    es.onopen = () => (connected = true);
    es.onerror = () => (connected = false);
    es.onmessage = (ev) => {
      try {
        applyUpdate(JSON.parse(ev.data));
      } catch (e) {
        console.error('parse error', e);
      }
    };
  });

  onDestroy(() => {
    es?.close();
    map?.remove();
  });
</script>

<div class="flex h-screen w-screen">
  <aside class="w-72 shrink-0 border-r border-slate-800 bg-slate-900/80 p-4 overflow-y-auto">
    <h1 class="text-lg font-semibold tracking-tight">Transit Ops Center</h1>
    <p class="mt-1 text-xs text-slate-400">Live Bay Area vehicle positions · GTFS-RT via 511.org</p>

    <div class="mt-4 flex items-center gap-2 text-xs">
      <span class="inline-flex h-2 w-2 rounded-full {connected ? 'bg-emerald-400' : 'bg-rose-500'}"></span>
      <span class="text-slate-300">{connected ? 'streaming' : 'disconnected'}</span>
      {#if lastUpdate}
        <span class="ml-auto text-slate-500">{new Date(lastUpdate).toLocaleTimeString()}</span>
      {/if}
    </div>

    <div class="mt-3 rounded-md border border-slate-800 bg-slate-950/40 p-2 text-[11px] font-mono leading-relaxed">
      <div>map: <span class="text-slate-300">{mapStatus}</span></div>
      {#if mapError}
        <div class="text-rose-400 break-words">err: {mapError}</div>
      {/if}
    </div>

    <div class="mt-4 rounded-md bg-slate-800/60 p-3">
      <div class="text-2xl font-semibold">{vehicles.length}</div>
      <div class="text-xs uppercase tracking-wide text-slate-400">vehicles tracked</div>
    </div>

    <h2 class="mt-6 text-xs font-semibold uppercase tracking-wide text-slate-400">Routes</h2>
    <ul class="mt-2 space-y-1 text-sm">
      {#each routeCounts as [route, count]}
        <li class="flex items-center gap-2">
          <span class="h-3 w-3 rounded-full" style="background: {colorForRoute(route)}"></span>
          <span class="text-slate-200">{route}</span>
          <span class="ml-auto text-slate-500">{count}</span>
        </li>
      {/each}
    </ul>
  </aside>

  <main class="relative flex-1">
    <div bind:this={mapEl} class="absolute inset-0" style="width: 100%; height: 100%; background: #0f172a;"></div>
  </main>
</div>

<style>
  /* Leaflet's default attribution control uses light colors that don't fit dark theme */
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
    box-shadow: none;
  }
  :global(.leaflet-tooltip-top:before) {
    border-top-color: #334155;
  }
</style>
