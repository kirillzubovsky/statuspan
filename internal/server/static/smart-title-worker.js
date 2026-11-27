// Same-origin title worker. The page posts short titles; this file does not.
import { env, pipeline } from "/api/smart-title/lib/transformers.web.js";

var MODEL = "onnx-community/Qwen2.5-0.5B-Instruct";
var SYSTEM = "Reply with only a 6-10 word job title. No quotes. No trailing period. No prefix.";

env.allowLocalModels = false;
env.allowRemoteModels = true;
env.remoteHost = new URL("/api/smart-title/model/", self.location.href).href;
env.remotePathTemplate = "{model}/resolve/main/";
if (env.backends && env.backends.onnx && env.backends.onnx.wasm) {
  env.backends.onnx.wasm.numThreads = 1;
}

var pending = [];
var busy = false;
var generator = null;
var loading = null;
var stopped = false;

async function webGPUAdapter() {
  try {
    if (!self.navigator || !self.navigator.gpu || typeof self.navigator.gpu.requestAdapter !== "function") {
      return null;
    }
    return await self.navigator.gpu.requestAdapter();
  } catch (err) {
    return null;
  }
}

function opts(device, dtype) {
  return {
    device: device,
    dtype: dtype,
    progress_callback: function (p) {
      var progress = null;
      if (p && typeof p.progress === "number") progress = p.progress;
      postMessage({
        type: "progress",
        status: p && p.status ? String(p.status) : "",
        progress: progress
      });
    }
  };
}

function contentOf(message) {
  if (!message) return "";
  if (typeof message === "string") return message;
  var content = message.content;
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content.map(function (part) {
    if (typeof part === "string") return part;
    if (part && typeof part.text === "string") return part.text;
    return "";
  }).join("");
}

function textOf(out) {
  var item = out;
  if (Array.isArray(out)) item = out[0];
  if (!item) return "";
  var gen = item.generated_text;
  if (typeof gen === "string") return gen;
  if (Array.isArray(gen) && gen.length) return contentOf(gen[gen.length - 1]);
  return "";
}

function load(forceWasm) {
  if (stopped) return Promise.resolve(null);
  if (generator) return Promise.resolve(generator);
  if (loading) return loading;
  loading = (async function () {
    var adapter = null;
    if (!forceWasm) adapter = await webGPUAdapter();
    if (adapter) {
      try {
        generator = await pipeline("text-generation", MODEL, opts("webgpu", "q4f16"));
      } catch (err) {
        // A failed WebGPU init taints ONNX Runtime in this worker.
        stopped = true;
        generator = null;
        loading = null;
        postMessage({ type: "fallback-wasm", message: String(err && err.message || err) });
        return null;
      }
    } else {
      generator = await pipeline("text-generation", MODEL, opts("wasm", "uint8"));
    }
    postMessage({ type: "ready" });
    return generator;
  })().catch(function (err) {
    loading = null;
    generator = null;
    postMessage({ type: "error", message: String(err && err.message || err) });
    throw err;
  });
  return loading;
}

async function summarize(msg) {
  var messages = [
    { role: "system", content: SYSTEM },
    { role: "user", content: String(msg.title || "") }
  ];
  var out = await generator(messages, {
    max_new_tokens: 24,
    do_sample: false,
    temperature: 0
  });
  return textOf(out);
}

function pump() {
  if (busy || !generator || !pending.length) return;
  var msg = pending.shift();
  busy = true;
  summarize(msg).then(function (text) {
    postMessage({
      type: "result",
      id: msg.id,
      agent: msg.agent,
      jobId: msg.jobId,
      raw: String(msg.title || ""),
      text: text
    });
  }).catch(function () {
    postMessage({
      type: "fail",
      id: msg.id,
      agent: msg.agent,
      jobId: msg.jobId,
      raw: String(msg.title || "")
    });
  }).then(function () {
    busy = false;
    pump();
  });
}

self.onmessage = function (ev) {
  if (stopped) return;
  var msg = ev.data || {};
  if (msg.type === "load") {
    load(!!msg.forceWasm).catch(function () {});
    return;
  }
  if (msg.type === "summarize") {
    pending.push(msg);
    if (generator) pump();
    else load(false).then(function () { pump(); }).catch(function () {});
  }
};
