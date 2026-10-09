// SPDX-License-Identifier: AGPL-3.0-only

// Draws the charts pages describe (a .chart with its data as JSON), with
// Chart.js, in the site's colours. It draws any that appear later too (the
// console swaps pages without reloading), and draws them again when the
// theme changes between light and dark.
(function () {
  // Series colours: the teams' colours from the tokens, which read in the
  // theme being shown.
  var fallback = ["#4c55d8", "#0f7d69", "#9a5d06", "#b8386a", "#1f6fb8", "#7448c2", "#3f7a1c", "#b0471f"];
  var charts = [];

  function css(name, fallback) {
    var v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
    return v || fallback;
  }
  function fmt(data, v) {
    if (v === null || v === undefined) return "";
    if (data.money) return "$" + Number(v).toFixed(2);
    return Number(v).toLocaleString() + (data.unit ? " " + data.unit : "");
  }

  function config(data, narrow) {
    var text = css("--muted", "#888"), line = css("--line", "#ddd"), quiet = css("--faint", "#999");
    var bar = data.kind === "bar" || data.kind === "hbar";
    var palette = fallback.map(function (c, i) { return css("--team-" + i, c); });
    // A line through one or two days is only its points: show them.
    var few = (data.labels || []).length < 3;
    var n = 0;
    var datasets = data.series.map(function (s) {
      var colour = s.quiet ? quiet : palette[n++ % palette.length];
      var ds = {
        label: s.name, data: s.values, borderColor: colour, backgroundColor: colour,
        borderWidth: 2, pointRadius: few ? 3 : 0, pointHoverRadius: 4, spanGaps: false
      };
      if (s.dashed) ds.borderDash = [6, 4];
      if (bar && s.line) {
        ds.type = "line";
        ds.stack = "line-" + s.name; // not stacked onto the bars
        ds.order = -1;               // drawn over them
      } else if (bar) {
        ds.borderWidth = 0;
        ds.borderRadius = 3;
        ds.stack = "bars";
        if (s.quiet) ds.backgroundColor = css("--field", "rgba(128,128,128,.5)");
      }
      if (s.right) ds.yAxisID = "right";
      return ds;
    });
    var valueAxis = {
      beginAtZero: true, stacked: bar, grid: { color: line }, border: { display: false },
      ticks: { color: text, callback: function (v) { return fmt(data, v); } }
    };
    var labelAxis = { stacked: bar, grid: { display: false }, ticks: { color: text, autoSkip: true, maxRotation: 0, maxTicksLimit: data.kind === "hbar" ? undefined : narrow ? 5 : 10 } };
    var scales = data.kind === "hbar" ? { x: valueAxis, y: labelAxis } : { x: labelAxis, y: valueAxis };
    if (data.series.some(function (s) { return s.right; })) {
      scales.right = { position: "right", beginAtZero: true, grid: { display: false }, border: { display: false },
        ticks: { color: text, precision: 0 } };
    }
    return {
      type: data.kind === "line" ? "line" : "bar",
      data: { labels: data.labels, datasets: datasets },
      options: {
        indexAxis: data.kind === "hbar" ? "y" : "x",
        responsive: true, maintainAspectRatio: false, animation: false,
        interaction: { mode: "index", intersect: false },
        scales: scales,
        plugins: {
          // Lines as short lines, bars as squares; never a dashed, torn box.
          legend: { labels: { color: text, usePointStyle: true, boxWidth: 16, boxHeight: 10,
            generateLabels: function (chart) {
              return Chart.defaults.plugins.legend.labels.generateLabels(chart).map(function (item) {
                var ds = chart.data.datasets[item.datasetIndex];
                var line = data.kind === "line" || ds.type === "line";
                item.pointStyle = line ? "line" : "rectRounded";
                item.lineDash = [];
                item.lineWidth = line ? 3 : 0;
                item.fillStyle = ds.backgroundColor;
                return item;
              });
            } } },
          tooltip: {
            filter: function (item) { return item.raw !== null && item.raw !== undefined; },
            callbacks: {
              label: function (item) {
                var s = data.series[item.datasetIndex];
                return " " + item.dataset.label + ": " + (s && s.right ? item.raw : fmt(data, item.raw));
              }
            }
          }
        }
      }
    };
  }

  function draw(el) {
    if (el.dataset.drawn || !window.Chart) return;
    var src = el.querySelector('script[type="application/json"]'), canvas = el.querySelector("canvas");
    if (!src || !canvas) return;
    var data;
    try { data = JSON.parse(src.textContent); } catch (e) { return; }
    Chart.defaults.font.family = getComputedStyle(document.body).fontFamily;
    el.dataset.drawn = "1";
    charts.push({ el: el, chart: new Chart(canvas, config(data, el.clientWidth < 500)) });
  }

  function drawAll() {
    // Forget charts whose page was swapped out.
    charts = charts.filter(function (c) {
      if (document.contains(c.el)) return true;
      c.chart.destroy();
      return false;
    });
    document.querySelectorAll(".chart[data-chart]").forEach(draw);
  }

  function redrawAll() {
    charts.forEach(function (c) { c.chart.destroy(); delete c.el.dataset.drawn; });
    charts = [];
    drawAll();
  }

  function start() {
    drawAll();
    new MutationObserver(drawAll).observe(document.body, { childList: true, subtree: true });
    new MutationObserver(redrawAll).observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
    if (window.matchMedia) {
      var dark = window.matchMedia("(prefers-color-scheme: dark)");
      if (dark.addEventListener) dark.addEventListener("change", redrawAll);
    }
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start);
  else start();
})();
