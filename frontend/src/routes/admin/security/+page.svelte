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
	// Security admin page (task 29.5, Req 33.8). Renders the tenant Two-Factor
	// policy toggle inside the admin-gated layout.
	import TwoFactorPolicyToggle from '$lib/components/admin/TwoFactorPolicyToggle.svelte';
</script>

<TwoFactorPolicyToggle />
