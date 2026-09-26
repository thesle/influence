<script lang="ts">
	// Whiteboard_Polygon view (Req 10.1). A freeform canvas surface. A full
	// drawing engine is out of scope for this task; this provides a working
	// canvas placeholder with basic pen strokes so the type renders and is
	// interactive, leaving richer tooling for a later iteration.

	import type { Polygon } from '$lib/polygonApi';

	interface Props {
		// The polygon is accepted for parity with the other views; the canvas does
		// not yet persist strokes to content, so it is intentionally unused.
		polygon: Polygon;
	}

	// eslint-disable-next-line @typescript-eslint/no-unused-vars
	let _props: Props = $props();

	let canvas = $state<HTMLCanvasElement | null>(null);
	let drawing = false;

	function ctx(): CanvasRenderingContext2D | null {
		return canvas?.getContext('2d') ?? null;
	}

	function pos(e: PointerEvent): { x: number; y: number } {
		const rect = canvas!.getBoundingClientRect();
		return { x: e.clientX - rect.left, y: e.clientY - rect.top };
	}

	function down(e: PointerEvent) {
		const c = ctx();
		if (!c) return;
		drawing = true;
		const { x, y } = pos(e);
		c.beginPath();
		c.moveTo(x, y);
	}
	function move(e: PointerEvent) {
		if (!drawing) return;
		const c = ctx();
		if (!c) return;
		const { x, y } = pos(e);
		c.lineTo(x, y);
		c.strokeStyle = '#1f2933';
		c.lineWidth = 2;
		c.stroke();
	}
	function up() {
		drawing = false;
	}

	function clear() {
		const c = ctx();
		if (c && canvas) c.clearRect(0, 0, canvas.width, canvas.height);
	}
</script>

<section class="whiteboard" data-testid="whiteboard-polygon">
	<div class="controls">
		<button type="button" onclick={clear}>Clear</button>
		<span class="note">Freeform whiteboard (canvas)</span>
	</div>
	<canvas
		bind:this={canvas}
		width="900"
		height="500"
		onpointerdown={down}
		onpointermove={move}
		onpointerup={up}
		onpointerleave={up}
	></canvas>
</section>

<style>
	.controls {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		margin-bottom: 0.5rem;
	}
	.controls button {
		border: 1px solid #cbd2d9;
		background: #fff;
		border-radius: 4px;
		padding: 0.3rem 0.75rem;
		cursor: pointer;
	}
	.note {
		color: #7b8794;
		font-size: 0.85rem;
	}
	canvas {
		border: 1px solid #cbd2d9;
		border-radius: 4px;
		background: #fff;
		touch-action: none;
		max-width: 100%;
	}
</style>
