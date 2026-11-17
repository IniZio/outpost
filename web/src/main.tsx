import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Route, Routes } from "react-router-dom";
import { Home } from "./Home";
import { Layout } from "./Layout";
import { Run } from "./Run";
import { Workflow } from "./Workflow";
import "./app.css";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <BrowserRouter>
      <Routes>
        <Route element={<Layout />}>
          <Route path="/" element={<Home />} />
          <Route path="/workflows/:name" element={<Workflow />} />
          <Route path="/tasks/:id" element={<Run />} />
          <Route path="*" element={<p className="mut">Nothing here. Go <a href="/">Home</a>.</p>} />
        </Route>
      </Routes>
    </BrowserRouter>
  </StrictMode>,
);
