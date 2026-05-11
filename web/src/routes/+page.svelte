<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import maplibregl, { type Map as MlMap, type Marker } from 'maplibre-gl';
  import 'maplibre-gl/dist/maplibre-gl.css';

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

  let mapEl: HTMLDivElement;
  let map: MlMap | undefined;
  let markers = new Map<string, Marker>();
  let vehicles: Vehicle[] = $state([]);
  let connected = $state(false);
  let lastUpdate = $state<number | null>(null);
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

  function renderMarker(v: Vehicle) {
    const existing = markers.get(v.id);
    if (existing) {
      existing.setLngLat([v.lon, v.lat]);
      if (v.bearing != null) existing.setRotation(v.bearing);
      return;
    }
    const el = document.createElement('div');
    el.style.cssText = `
      width: 14px; height: 14px; border-radius: 50%;
      background: ${colorForRoute(v.route)};
      border: 2px solid white; box-shadow: 0 0 4px rgba(0,0,0,0.6);
      cursor: pointer;
    `;
    el.title = `${v.route ?? 'unknown'} • ${v.id}`;
    const m = new maplibregl.Marker({ element: el })
      .setLngLat([v.lon, v.lat])
      .addTo(map!);
    if (v.bearing != null) m.setRotation(v.bearing);
    markers.set(v.id, m);
  }

  function applyUpdate(next: Vehicle[]) {
    vehicles = next;
    lastUpdate = Date.now();
    const seen = new Set<string>();
    for (const v of next) {
      seen.add(v.id);
      renderMarker(v);
    }
    for (const [id, m] of markers) {
      if (!seen.has(id)) {
        m.remove();
        markers.delete(id);
      }
    }
  }

  onMount(async () => {
    map = new maplibregl.Map({
      container: mapEl,
      style: 'https://demotiles.maplibre.org/style.json',
      center: [-122.4194, 37.7749],
      zoom: 11
    });

    try {
      const r = await fetch('/api/vehicles');
      if (r.ok) applyUpdate(await r.json());
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

    <div class="mt-6 rounded-md bg-slate-800/60 p-3">
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
    <div bind:this={mapEl} class="absolute inset-0"></div>
  </main>
</div>
