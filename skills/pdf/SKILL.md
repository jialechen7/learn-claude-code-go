---
name: pdf
description: Process PDF files - extract text, create PDFs, merge documents. Use when user asks to read PDF, create PDF, or work with PDF files.
tags: files, documents
---

# PDF Processing Skill

You now have expertise in PDF manipulation.

## Reading PDFs

```bash
# Using pdftotext (poppler-utils)
pdftotext input.pdf -        # stdout
pdftotext input.pdf out.txt  # file
```

Or with Go:
```bash
go get github.com/ledongthuc/pdf
```

```go
import "github.com/ledongthuc/pdf"

func readPDF(path string) (string, error) {
    f, r, err := pdf.Open(path)
    defer f.Close()
    if err != nil { return "", err }
    var buf bytes.Buffer
    b, err := r.GetPlainText()
    if err != nil { return "", err }
    buf.ReadFrom(b)
    return buf.String(), nil
}
```

## Creating PDFs

```bash
# From HTML using wkhtmltopdf
wkhtmltopdf input.html output.pdf

# From markdown using pandoc
pandoc input.md -o output.pdf
```

## Merging PDFs

```bash
# Using pdfunite (poppler-utils)
pdfunite file1.pdf file2.pdf merged.pdf

# Using ghostscript
gs -dBATCH -dNOPAUSE -q -sDEVICE=pdfwrite -sOutputFile=merged.pdf file1.pdf file2.pdf
```
