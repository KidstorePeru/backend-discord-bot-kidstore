package discordbot

import (
	"bytes"
	_ "embed"

	"github.com/bwmarrin/discordgo"
)

// El ícono de KidCoin se embebe directamente en el binario y se manda como
// adjunto en cada mensaje — así no depende de que kidstoreperu.net esté
// desplegado con la última versión de la imagen (Discord además cachea
// agresivamente las imágenes por URL, así que actualizar el archivo en el
// sitio no siempre se refleja de inmediato).
//
//go:embed assets/kidcoin.png
var kidcoinPNG []byte

const kidcoinAttachment = "attachment://kidcoin.png"

func kidcoinFile() *discordgo.File {
	return &discordgo.File{Name: "kidcoin.png", ContentType: "image/png", Reader: bytes.NewReader(kidcoinPNG)}
}
