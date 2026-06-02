---
name: data-analysis
description: Analyze data using Python and SQL tools. Use when the user asks to process CSVs, Excel, Parquet, PDFs, Word docs, or PowerPoint files, query databases (PostgreSQL, MySQL, SQLite, SQL Server, BigQuery), create charts or visualizations, or perform any data analysis. For live dashboards or data apps, read the developing-software skill.
license: Complete terms in LICENSE.txt
---

# Data Analysis

## Python Environment Setup

Install data analysis packages on demand. Initialize a Python project in the workspace if one does not exist:

```bash
uv init --python 3.13
uv add pandas numpy polars matplotlib altair plotly seaborn scipy scikit-learn duckdb pyarrow jupyterlab
```

Run scripts and tools with `uv run`:
```bash
uv run python script.py
uv run jupyter nbconvert --to notebook --execute --inplace notebook.ipynb
```

Add more packages with `uv add <package>`. The project's `pyproject.toml` and `.venv` persist across sessions. Skip `uv init` if `pyproject.toml` already exists.

## Database CLI

### usql - Universal SQL CLI

```bash
# PostgreSQL
usql postgres://user:pass@host:5432/dbname

# MySQL
usql mysql://user:pass@host:3306/dbname

# SQLite
usql sqlite:./data.db

# SQL Server
usql sqlserver://user:pass@host/instance/dbname

# BigQuery — use Python client with access token instead (see BigQuery section below)
# usql bigquery:// is NOT recommended; use the google-cloud-bigquery Python client
```

Common commands inside usql:
- `\dt` - List tables
- `\d tablename` - Describe table
- `\q` - Quit

### sqlite3

```bash
sqlite3 data.db "SELECT * FROM users LIMIT 10"
```

## Python Data Processing

### Core Libraries

| Package | Purpose | Status |
|---------|---------|--------|
| `pandas` | DataFrames and data manipulation | install with `uv add` |
| `numpy` | Numerical computing | install with `uv add` |
| `polars` | Fast DataFrame library (Rust-based) | install with `uv add` |
| `duckdb` | In-process SQL analytics | install with `uv add` |

```python
import pandas as pd
import polars as pl
import duckdb

# Pandas
df = pd.read_csv("data.csv")
df.groupby("category").sum()

# Polars (faster for large data)
df = pl.read_csv("data.csv")
df.group_by("category").agg(pl.sum("amount"))

# DuckDB - SQL on files directly
result = duckdb.sql("SELECT * FROM 'data.csv' WHERE amount > 1000")
print(result.df())
```

### Visualization

camelAI's notebook preview renders Altair and Plotly charts natively — not in iframes. Chart colors, text, and backgrounds automatically adapt to the user's light/dark theme.

**Preferred order:**
1. **Altair** (Vega-Lite) — emits structured specs with full theme support
2. **Plotly** — also renders natively; use when Altair doesn't cover the chart type (3D, maps, financial)
3. **matplotlib / seaborn** — static PNG fallback; won't adapt to dark mode

| Package | Purpose | Status |
|---------|---------|--------|
| `altair` | Declarative charts (Vega-Lite) — **preferred** | install with `uv add` |
| `plotly` | Interactive charts — use for 3D, maps, finance | install with `uv add` |
| `matplotlib` | Static plots (fallback) | install with `uv add` |
| `seaborn` | Statistical visualization (fallback) | install with `uv add` |

```python
# Altair (preferred — renders natively with dark/light theme support)
import altair as alt

chart = alt.Chart(df).mark_bar().encode(
    x="category:N",
    y="amount:Q"
).properties(
    title=alt.Title("Sales by Category", subtitle="Q4 2025 data"),
    width=500,
    height=300
)
chart  # Display in notebook cell output

# Plotly (native rendering, use for charts Altair doesn't support)
import plotly.express as px
fig = px.line(df, x="date", y="value", title="Trend Over Time")
fig.show()

# Matplotlib/Seaborn (static PNG — no dark mode support)
import matplotlib.pyplot as plt
import seaborn as sns
sns.barplot(data=df, x="category", y="amount")
plt.savefig("barplot.png")
```

**Altair renderer constraints:**
- Use `alt.Title("Title", subtitle="Subtitle")` — both are themed automatically
- Do **not** set `background` — the renderer makes backgrounds transparent
- Do **not** hardcode text colors — the renderer applies theme-appropriate colors
- Set `width`/`height` via `.properties()` — width is overridden to fill the container; height is used as a baseline
- Arc marks (donut/pie) are detected and allocated extra vertical space automatically

**Plotly renderer constraints:**
- Use `fig.show()` to emit Plotly MIME output — the renderer picks it up natively
- Do **not** use `fig.write_image()` or `fig.write_html()` — these bypass native rendering
- Do **not** set `paper_bgcolor` or `plot_bgcolor` — the renderer makes them transparent
- Subtitles via `layout.annotations` are automatically themed

### Tabular output

When outputting tabular data in notebooks, use plain pandas DataFrames — not `df.style` (pandas Styler). The rendering environment handles table styling automatically with theme-aware colors, index columns, and overflow handling.

**Never output tables as raw HTML** (e.g., manually constructing `<table>` tags or using `IPython.display.HTML("<table>...")`). Always use pandas DataFrames for tabular output — the rendering environment detects DataFrames automatically and applies theme-aware styling, sortable columns, row filtering, and CSV export. Raw HTML tables bypass all of this and render unstyled in an iframe.

Set pandas display options explicitly only when the user asks for different table display limits.

Only use `df.style` when the user explicitly requests conditional formatting, cell-level color coding, or other per-cell visual logic that can't be achieved with a plain table.

## Jupyter Notebook Workflow (Preferred)

For exploratory analysis, deliver results as a Jupyter notebook (`.ipynb`).

**Do not** deliver results as a:
- standalone `.py` script with separate chart/image files 
- html file
unless explicitly requested by the user. 

Notebooks preview reliabily with rich Altair charts and markdown rendering, and are better for report consumption. They combine code, visual output, and markdown conclusions in one artifact. 

### Build notebooks incrementally

- Keep a narrative flow:
  - markdown cell: objective and dataset context
  - code cell: data loading/cleaning
  - markdown cell: what to look for
  - code cell: chart/query
  - markdown cell: interpretation and takeaway

### Execute notebooks

```bash
uv run jupyter nbconvert --to notebook --execute --inplace analysis.ipynb
```

Setup calls whose return value is not meaningful report content, such as `alt.data_transformers.disable_max_rows()` or `plt.plot(...)`, should be silenced with a trailing `;` or assigned to `_` so object reprs do not leak into notebook outputs.

**Always validate after execution.** Run the notebook validator to catch errors that `nbconvert` may not surface (cell exceptions, charts that fell back to text/plain, blank charts with constant data):

```bash
validate-notebook analysis.ipynb
```

If it reports issues, fix the failing cells and re-execute. Do **not** use `--allow-errors` — it silently embeds tracebacks in cell outputs that the user will see in the rendered report.

### Preview notebooks in chat

After creating or updating a notebook, set the active chat preview to the notebook file:

```text
set_preview(
  path="/home/claude/analysis.ipynb",
  content_type="application/x-ipynb+json"
)
```

### Publish files as standalone apps

When a user wants to publish a notebook (or any file) as a standalone app, deploy with:

```bash
publish my-notebook-app --file /path/to/analysis.ipynb
```

This deploys a lightweight Cloudflare Worker that serves the file via the main app's embed viewer. No build step required.

### How notebooks are presented

camelAI renders notebooks in **Report mode** by default — the user sees a polished article, not raw cells.

**What Report mode does:**
- Hides all code — only markdown prose and cell outputs (charts, tables, text) are visible
- Auto-hides setup cells (imports, data loading, `.describe()`, `pd.set_option`, etc.)
- Extracts the first `#` heading as the report title and the following paragraph as the subtitle
- Builds a sidebar table of contents from `##` and `###` headings

**Structure notebooks for Report mode:**
- Start with a single `#` heading followed by a one-sentence description (becomes the report header)
- Use `##` headings to define sections — these populate the sidebar TOC
- Keep setup code in dedicated cells (the classifier hides entire cells, not individual lines)
- Write markdown between analysis cells explaining what each result shows
- End with a `## Key Findings` or `## Conclusion` section

The user can toggle to Notebook mode to see all cells, code, and execution counts, but Report mode is the default first impression.

## Scientific Computing & ML

| Package | Purpose | Status |
|---------|---------|--------|
| `scipy` | Scientific computing, optimization | install with `uv add` |
| `scikit-learn` | Machine learning algorithms | install with `uv add` |

```python
from sklearn.model_selection import train_test_split
from sklearn.linear_model import LinearRegression

X_train, X_test, y_train, y_test = train_test_split(X, y, test_size=0.2)
model = LinearRegression().fit(X_train, y_train)
predictions = model.predict(X_test)
```

## Database Connectivity

| Package | Purpose | Status |
|---------|---------|--------|
| `sqlalchemy` | Python ORM and database toolkit | install with `uv add` |
| `psycopg` | PostgreSQL driver | install with `uv add` |
| `pymysql` | MySQL driver | `uv add pymysql` |
| `google-cloud-bigquery` | BigQuery client | install with `uv add` |
| `google-cloud-bigquery-storage` | BigQuery Storage API client for faster downloads | install with `uv add` |
| `google-auth` | Google authentication (used for BigQuery tokens) | install with `uv add` |

### SQL Server / PostgreSQL / MySQL (Primary: Worker `DATA_PROXY` service binding)

For deployed/user-uploaded Cloudflare Workers, use the `DATA_PROXY` service binding first.
This is the most important path because Workers may not be able to use native DB drivers/TCP connectivity directly.

Read example:

```typescript
const readResult = await context.cloudflare.env.DATA_PROXY.postgresQuery({
  mode: "read",
  host: "db.example.com",
  user: "user",
  password: "pass",
  database: "analytics",
  query: "SELECT id, email FROM users WHERE id = $1 LIMIT 100",
  params: [123],
});

if (!readResult.ok) throw new Error(readResult.error.message);
const rows = readResult.data.recordset ?? [];
```

Modify example:

```typescript
const modifyResult = await context.cloudflare.env.DATA_PROXY.postgresQuery({
  mode: "modify",
  host: "db.example.com",
  user: "user",
  password: "pass",
  database: "analytics",
  query: "UPDATE users SET last_seen_at = NOW() WHERE id = $1",
  params: [123],
});

if (!modifyResult.ok) throw new Error(modifyResult.error.message);
const affected = modifyResult.data.rowsAffected?.[0] ?? 0;
```

Supported query methods:
- `DATA_PROXY.mssqlQuery(...)` (named params, e.g. `@id`)
- `DATA_PROXY.postgresQuery(...)` (positional params array)
- `DATA_PROXY.mysqlQuery(...)` (positional params array)
- All query calls require `mode: "read"` or `mode: "modify"` (no auto-detection).

### Direct drivers (preferred local fallback in containers)

For sandbox/container code, native drivers are the primary fallback when proxy access is unnecessary.
Use this path when the user explicitly asks for direct connectivity or when local direct access is simpler.

```python
from sqlalchemy import create_engine
import pandas as pd

# PostgreSQL direct
pg_engine = create_engine("postgresql+psycopg://user:pass@host/db")
pg_df = pd.read_sql("SELECT * FROM users", pg_engine)

# MySQL direct
mysql_engine = create_engine("mysql+pymysql://user:pass@host/db")
mysql_df = pd.read_sql("SELECT * FROM orders", mysql_engine)
```

## File Formats

| Package | Purpose | Status |
|---------|---------|--------|
| `pyarrow` | Parquet, Arrow files | install with `uv add` |
| `openpyxl` | Excel (.xlsx) read/write | install with `uv add` |
| `xlsxwriter` | Excel creation with formatting | install with `uv add` |
| `pdfplumber` | PDF text and table extraction | install with `uv add` |
| `python-docx` | Word documents | install with `uv add` |
| `python-pptx` | PowerPoint files | install with `uv add` |

```python
# Read Excel
df = pd.read_excel("data.xlsx", sheet_name="Sheet1")

# Write Excel with formatting
df.to_excel("output.xlsx", index=False)

# Read Parquet
df = pd.read_parquet("data.parquet")

# Extract tables from PDF
import pdfplumber
with pdfplumber.open("report.pdf") as pdf:
    for page in pdf.pages:
        tables = page.extract_tables()
```

## Live Dashboards & Data Apps

When the user wants a **live dashboard**, **data app**, or any interactive web UI built on top of their database or data sources, use the `developing-software` skill. That skill covers deploying fullstack Cloudflare Workers apps with React, Vite, and shadcn/ui — which is the right approach for persistent, shareable dashboards.

**Read the `developing-software` skill** before building any dashboard or data-driven web app. It documents:
- `create-worker` for scaffolding React + Vite projects
- Durable Objects with SQLite for server-side state
- shadcn/ui components for charts, tables, and UI
- Deployment via `bun deploy`

Database connection credentials are available through the virtual connections binding. Prefer connection methods over direct credential handling.

## Additional Packages

| Package | Purpose | Status |
|---------|---------|--------|
| `statsmodels` | Statistical modeling, time series | install with `uv add` |
| `xgboost` | Gradient boosting | install with `uv add` |
| `geopandas` | Geospatial data | `uv add geopandas` |
| `opencv-python-headless` | Computer vision | `uv add opencv-python-headless` |
