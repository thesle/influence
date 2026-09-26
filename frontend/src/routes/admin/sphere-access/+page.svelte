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
	// Sphere-access admin page (task 23.3, Req 5.3). Renders the access matrix
	// (access level + Reveal per Group per Sphere) inside the admin-gated layout.
	import SphereAccessMatrix from '$lib/components/admin/SphereAccessMatrix.svelte';
</script>

<SphereAccessMatrix />
