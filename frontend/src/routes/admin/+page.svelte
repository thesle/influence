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
	// Admin landing (task 23.3). The admin section is organised into tabbed
	// sub-pages (Users, Groups, Sphere access); this index simply points at them.
	// It renders inside the /admin layout, so it is only reachable by admins.
</script>

<section class="admin-home">
	<p>Choose a section to manage users, groups, or per-Sphere access.</p>
	<ul>
		<li><a href="/admin/users">Users</a> — create, edit, and deactivate users.</li>
		<li><a href="/admin/groups">Groups</a> — create and rename groups.</li>
		<li>
			<a href="/admin/sphere-access">Sphere access</a> — set access level and Reveal per group.
		</li>
		<li>
			<a href="/admin/security">Security</a> — set the tenant two-factor authentication policy.
		</li>
	</ul>
</section>

<style>
	.admin-home ul {
		line-height: 1.7;
	}
</style>
