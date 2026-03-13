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
  const THEME_KEY = "abp_theme";
  const themeToggle = document.getElementById("theme-toggle");
  function getTheme() {
    return localStorage.getItem(THEME_KEY) || "light";
  }
  function setTheme(theme) {
    if (theme === "light") {
      document.documentElement.setAttribute("data-theme", "light");
    } else {
      document.documentElement.removeAttribute("data-theme");
    }
    localStorage.setItem(THEME_KEY, theme);
    if (themeToggle) {
      themeToggle.innerHTML = theme === "light"
        ? '<svg class="theme-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/></svg>'
        : '<svg class="theme-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="5"/><path d="M12 1v2M12 21v2M4.22 4.22l1.42 1.42M18.36 18.36l1.42 1.42M1 12h2M21 12h2M4.22 19.78l1.42-1.42M18.36 5.64l1.42-1.42"/></svg>';
      themeToggle.title = theme === "light" ? "Тёмная тема" : "Светлая тема";
    }
  }
  setTheme(getTheme());
  if (themeToggle) {
    themeToggle.addEventListener("click", () => {
      setTheme(getTheme() === "light" ? "dark" : "light");
    });
  }

  const EXPAND_KEY = "abp_expand";
  const expandToggle = document.getElementById("expand-toggle");
  const mainContainer = document.querySelector("main.container");
  if (expandToggle && mainContainer) {
    const isExpanded = () => localStorage.getItem(EXPAND_KEY) === "1";
    const setExpanded = (expanded) => {
      if (expanded) {
        mainContainer.classList.add("container-expanded");
        expandToggle.innerHTML = '<svg class="expand-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M9 3H3v6M15 21h6v-6M3 15l7-7M21 9l-7 7"/></svg>';
        expandToggle.title = "Свернуть";
      } else {
        mainContainer.classList.remove("container-expanded");
        expandToggle.innerHTML = '<svg class="expand-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M15 3h6v6M9 21H3v-6M21 3l-7 7M3 21l7-7"/></svg>';
        expandToggle.title = "Расширить";
      }
      localStorage.setItem(EXPAND_KEY, expanded ? "1" : "0");
    };
    setExpanded(isExpanded());
    expandToggle.addEventListener("click", () => setExpanded(!isExpanded()));
  }

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
