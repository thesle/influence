<script lang="ts">
	// Documentation index (task 23.3, Req 27.2). Entry point into the in-app
	// docs; individual documents render at /docs/{path} via the catch-all route.
	// The default landing document is "index"; deep links point at specific docs.
</script>

<section class="docs-index">
	<h1>Documentation</h1>
	<p>Browse the platform documentation without leaving the app.</p>
	<ul>
		<li><a href="/docs/index">Overview</a></li>
		<li><a href="/docs/getting-started">Getting started</a></li>
	</ul>
</section>

<style>
	.docs-index ul {
		line-height: 1.7;
	}
</style>
