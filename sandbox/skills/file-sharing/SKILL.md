---
name: file-sharing
description: Exchange files with users through the camelAI chat interface. Read files they upload and create downloadable/previewable files for them.
license: Complete terms in LICENSE.txt
---

This skill enables file exchange between you and the user through camelAI's chat interface.

## User Uploads

Users can upload files by dragging and dropping onto the chat or clicking the + button. When they upload a file, you'll see a message like:

```
(user uploaded file to /mnt/user-uploads/document-1736712345-abc123.pdf)
```

### Reading User Uploads

To access the uploaded file:

```bash
# List uploaded files
ls /mnt/user-uploads/

# Read a specific file
cat /mnt/user-uploads/filename.txt

# Read an image (for processing)
file /mnt/user-uploads/image.png
```

Files persist across sessions, so users can reference previously uploaded files.

## Creating Output Files

To create a file the user can download or preview, save it to `/mnt/user-outputs/`:

```bash
# Save a text file
echo "Report content here" > /mnt/user-outputs/report.txt

# Copy a generated file
cp output.pdf /mnt/user-outputs/report.pdf

# Create subdirectories if needed
mkdir -p /mnt/user-outputs/charts
cp chart.png /mnt/user-outputs/charts/analysis.png
```

### Providing Links

After saving a file, provide a URL so the user can access it. The URL format uses the workspace outputs API. Check your system prompt for the exact URL pattern with your workspace ID.

**For images** - Use markdown image syntax for inline preview:
```markdown
![Chart Description](/api/workspaces/{workspace-id}/outputs/chart.png)
```

**For downloads** - Use markdown link syntax:
```markdown
[Download Report](/api/workspaces/{workspace-id}/outputs/report.pdf)
```

Images will display inline in the chat, other files will download when clicked.

**For HTML pages** - Save the file to `/mnt/user-outputs/` (or anywhere in `/home/claude/`) and call `set_preview()` to render it in the preview pane.

## Best Practices

1. **Confirm receipt** - When a user uploads a file, acknowledge it and briefly describe what you see.

2. **Use descriptive filenames** - When creating output files, use clear names like `sales-report-2024.pdf` instead of `output.pdf`.

3. **Always provide links** - Don't just say "I've saved the file". Provide a URL so users can easily access it.

4. **Use inline images** - For charts, diagrams, and visual outputs, use the image markdown syntax so users see them directly in the chat.

5. **Handle large files** - For very large outputs, consider creating a zip archive:
   ```bash
   zip -r /mnt/user-outputs/all-files.zip generated-files/
   ```
   Then provide a download link.

6. **Clean up** - If you create temporary files during processing, remove them when done. Only keep files in `/mnt/user-outputs/` that the user needs.

## Directory Structure

```
/mnt/
  user-uploads/     # Read-only for you - user's uploaded files
    document.pdf
    image.png
  user-outputs/     # Write here - files for user to download/preview
    report.pdf
    data.csv
    charts/
      analysis.png
```
