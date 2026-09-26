<!--
  Influence — a self-hostable documentation platform.
  Copyright (C) 2026  Conrad Smith

  This program is free software: you can redistribute it and/or modify
  it under the terms of the GNU General Public License as published by
  the Free Software Foundation, version 3.

  This program is distributed in the hope that it will be useful,
  but WITHOUT ANY WARRANTY; without even the implied warranty of
  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
  GNU General Public License for more details.

  You should have received a copy of the GNU General Public License
  along with this program.  If not, see <https://www.gnu.org/licenses/>.
-->

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
