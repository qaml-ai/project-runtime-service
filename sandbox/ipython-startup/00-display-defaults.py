"""
Default pandas display settings for camelAI notebooks.
Loaded automatically via IPython startup so notebook executions inherit these
defaults unless users explicitly override them.
"""

try:
    import pandas as pd

    pd.set_option("display.max_rows", 200)
    pd.set_option("display.min_rows", 200)
    pd.set_option("display.max_columns", 50)
    pd.set_option("display.max_colwidth", 1000)
    pd.set_option("display.width", None)
except ImportError:
    pass
