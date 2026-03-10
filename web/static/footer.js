/*
 * Copyright (C) 2026 GorillaHacker <gorillahacker@yandex.ru> https://t.me/gorillahacker
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

document.addEventListener("DOMContentLoaded", () => {
  const yearEl = document.getElementById("footer-year");
  if (yearEl) {
    yearEl.textContent = String(new Date().getFullYear());
  }

  const cpuApp = document.getElementById("footer-cpu-app");
  const cpuFree = document.getElementById("footer-cpu-free");
  const memApp = document.getElementById("footer-mem-app");
  const memFree = document.getElementById("footer-mem-free");
  const dbProject = document.getElementById("footer-db-project");
  const dbTotal = document.getElementById("footer-db-total");
  const dbFree = document.getElementById("footer-db-free");

  const hasFooter = cpuApp && cpuFree && memApp && memFree && dbProject && dbTotal && dbFree;
  if (!hasFooter) return;

  function formatPercent(value) {
    if (!Number.isFinite(value)) return "-";
    return `${value.toFixed(1)}%`;
  }

  function formatBytes(bytes) {
    if (!Number.isFinite(bytes) || bytes < 0) return "-";
    const units = ["B", "KB", "MB", "GB", "TB"];
    let size = bytes;
    let idx = 0;
    while (size >= 1024 && idx < units.length - 1) {
      size /= 1024;
      idx += 1;
    }
    return `${size.toFixed(1)} ${units[idx]}`;
  }

  function updateFooter(data) {
    cpuApp.textContent = formatPercent(data.cpu_process);
    cpuFree.textContent = formatPercent(data.cpu_free);
    memApp.textContent = formatPercent(data.mem_process);
    memFree.textContent = formatPercent(data.mem_free);
    dbProject.textContent = formatBytes(data.db_project_bytes);
    dbTotal.textContent = formatBytes(data.db_total_bytes);
    dbFree.textContent = formatBytes(data.db_free_bytes);
  }

  function loadMetrics() {
    const projectEl = document.querySelector("[data-project-id]");
    const projectId = projectEl ? projectEl.getAttribute("data-project-id") : "";
    const url = projectId ? `/api/metrics?project_id=${projectId}` : "/api/metrics";
    fetch(url)
      .then((res) => res.ok ? res.json() : null)
      .then((data) => {
        if (!data) return;
        updateFooter(data);
      })
      .catch(() => {});
  }

  loadMetrics();
  setInterval(loadMetrics, 5000);
});
