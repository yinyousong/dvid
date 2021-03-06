/*
	Package googlevoxels implements DVID support for multi-scale tiles and volumes in XY, XZ,
	and YZ orientation using the Google BrainMaps API.
*/
package googlevoxels

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"

	"code.google.com/p/go.net/context"

	"github.com/janelia-flyem/dvid/datastore"
	"github.com/janelia-flyem/dvid/datatype/multiscale2d"
	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/message"
	"github.com/janelia-flyem/dvid/server"
	"github.com/janelia-flyem/dvid/storage"
)

const (
	Version  = "0.1"
	RepoURL  = "github.com/janelia-flyem/dvid/datatype/googlevoxels"
	TypeName = "googlevoxels"
)

const HelpMessage = `
API for datatypes derived from googlevoxels (github.com/janelia-flyem/dvid/datatype/googlevoxels)
=================================================================================================

Command-line:

$ dvid repo <UUID> new googlevoxels <data name> <settings...>

	Adds voxel support using Google BrainMaps API.

	Example:

	$ dvid repo 3f8c new googlevoxels grayscale volumeid=281930192:stanford authkey=Jna3jrna984l

    Arguments:

    UUID           Hexidecimal string with enough characters to uniquely identify a version node.
    data name      Name of data to create, e.g., "mygrayscale"
    settings       Configuration settings in "key=value" format separated by spaces.

    Required Configuration Settings (case-insensitive keys)

    volumeid       The globally unique identifier of the volume within Google BrainMaps API.
    authkey        The API key required for Google BrainMaps API requests.

    Optional Configuration Settings (case-insensitive keys)

    tilesize       Default size in pixels along one dimension of square tile.  If unspecified, 512.


    ------------------

HTTP API (Level 2 REST):

GET  <api URL>/node/<UUID>/<data name>/help

	Returns data-specific help message.


GET  <api URL>/node/<UUID>/<data name>/info

    Retrieves characteristics of this data in JSON format.

    Example: 

    GET <api URL>/node/3f8c/grayscale/info

    Arguments:

    UUID          Hexidecimal string with enough characters to uniquely identify a version node.
    data name     Name of googlevoxels data.


GET  <api URL>/node/<UUID>/<data name>/tile/<dims>/<scaling>/<tile coord>[?options]

    Retrieves a tile of named data within a version node.  The default tile size is used unless
    the query string "tilesize" is provided.

    Example: 

    GET <api URL>/node/3f8c/grayscale/tile/xy/0/10_10_20

    Arguments:

    UUID          Hexidecimal string with enough characters to uniquely identify a version node.
    data name     Name of data to add.
    dims          The axes of data extraction in form "i_j_k,..."  Example: "0_2" can be XZ.
                    Slice strings ("xy", "xz", or "yz") are also accepted.
    scaling       Value from 0 (original resolution) to N where each step is downres by 2.
    tile coord    The tile coordinate in "x_y_z" format.  See discussion of scaling above.

  	Query-string options:

    tilesize      Size in pixels along one dimension of square tile.
  	noblanks	  If true, any tile request for tiles outside the currently stored extents
  				  will return a placeholder.
    format        "png", "jpeg" (default: "png")
                    jpeg allows lossy quality setting, e.g., "jpeg:80"  (0 <= quality <= 100)
                    png allows compression levels, e.g., "png:7"  (0 <= level <= 9)

GET  <api URL>/node/<UUID>/<data name>/raw/<dims>/<size>/<offset>[/<format>][?options]

    Retrieves raw image of named data within a version node using the Google BrainMaps API.

    Example: 

    GET <api URL>/node/3f8c/grayscale/raw/xy/512_256/0_0_100/jpg:80

    Arguments:

    UUID          Hexidecimal string with enough characters to uniquely identify a version node.
    data name     Name of data to add.
    dims          The axes of data extraction in form i_j.  Example: "0_2" can be XZ.
                    Slice strings ("xy", "xz", or "yz") are also accepted.
    size          Size in voxels along each dimension specified in <dims>.
    offset        Gives coordinate of first voxel using dimensionality of data.
    format        "png", "jpeg" (default: "png")
                    jpeg allows lossy quality setting, e.g., "jpeg:80"  (0 <= quality <= 100)
                    png allows compression levels, e.g., "png:7"  (0 <= level <= 9)

  	Query-string options:

  	scale         Default is 0.  For scale N, returns an image down-sampled by a factor of 2^N.
`

func init() {
	datastore.Register(NewType())

	// Need to register types that will be used to fulfill interfaces.
	gob.Register(&Type{})
	gob.Register(&Data{})
}

var (
	DefaultTileSize   int32  = 512
	DefaultTileFormat string = "png"
)

// Type embeds the datastore's Type to create a unique type with tile functions.
// Refinements of general tile types can be implemented by embedding this type,
// choosing appropriate # of channels and bytes/voxel, overriding functions as
// needed, and calling datastore.Register().
// Note that these fields are invariant for all instances of this type.  Fields
// that can change depending on the type of data (e.g., resolution) should be
// in the Data type.
type Type struct {
	datastore.Type
}

// NewDatatype returns a pointer to a new voxels Datatype with default values set.
func NewType() *Type {
	return &Type{
		datastore.Type{
			Name:    "googlevoxels",
			URL:     "github.com/janelia-flyem/dvid/datatype/googlevoxels",
			Version: "0.1",
			Requirements: &storage.Requirements{
				Batcher: true,
			},
		},
	}
}

// --- TypeService interface ---

// NewData returns a pointer to new googlevoxels data with default values.
func (dtype *Type) NewDataService(uuid dvid.UUID, id dvid.InstanceID, name dvid.DataString, c dvid.Config) (datastore.DataService, error) {
	// Make sure we have needed volumeid and authentication key.
	volumeid, found, err := c.GetString("volumeid")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("Cannot make googlevoxels data without valid 'volumeid' setting.")
	}
	authkey, found, err := c.GetString("authkey")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("Cannot make googlevoxels data without valid 'authkey' setting.")
	}

	// Make URL call to get the available scaled volumes.
	url := fmt.Sprintf("https://www.googleapis.com/brainmaps/v1beta1/volumes/%s?key=%s", volumeid, authkey)
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("Error getting volume metadata from Google: %s", err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Unexpected status code %d returned when getting volume metadata for %q", resp.StatusCode, volumeid)
	}
	metadata, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var m struct {
		Geoms Geometries `json:"geometrys"`
	}
	if err := json.Unmarshal(metadata, &m); err != nil {
		return nil, fmt.Errorf("Error decoding volume JSON metadata: %s", err.Error())
	}

	// Compute the mapping from tile scale/orientation to scaled volume index.
	tileMap := GeometryMap{}

	// (1) Find the highest resolution geometry.
	var highResIndex GeometryIndex
	minVoxelSize := dvid.NdFloat32{10000, 10000, 10000}
	for i, geom := range m.Geoms {
		if geom.PixelSize[0] < minVoxelSize[0] || geom.PixelSize[1] < minVoxelSize[1] || geom.PixelSize[2] < minVoxelSize[2] {
			minVoxelSize = geom.PixelSize
			highResIndex = GeometryIndex(i)
		}
	}
	dvid.Infof("Google voxels %q: found highest resolution was geometry %d: %s\n", name, highResIndex, minVoxelSize)

	// (2) For all geometries, find out what the scaling is relative to the highest resolution pixel size.
	for i, geom := range m.Geoms {
		if i == int(highResIndex) {
			tileMap[TileSpec{0, XY}] = highResIndex
			tileMap[TileSpec{0, XZ}] = highResIndex
			tileMap[TileSpec{0, YZ}] = highResIndex
		} else {
			scaleX := geom.PixelSize[0] / minVoxelSize[0]
			scaleY := geom.PixelSize[1] / minVoxelSize[1]
			scaleZ := geom.PixelSize[2] / minVoxelSize[2]
			var plane TileOrientation
			switch {
			case scaleX > scaleZ && scaleY > scaleZ:
				plane = XY
			case scaleX > scaleY && scaleZ > scaleY:
				plane = XZ
			case scaleY > scaleX && scaleZ > scaleX:
				plane = YZ
			default:
				dvid.Infof("Odd geometry skipped for Google voxels %q with pixel size: %s\n", name, geom.PixelSize)
				dvid.Infof("  Scaling from highest resolution: %d x %d x %d\n", scaleX, scaleY, scaleZ)
				continue
			}
			var mag float32
			if scaleX > mag {
				mag = scaleX
			}
			if scaleY > mag {
				mag = scaleY
			}
			if scaleZ > mag {
				mag = scaleZ
			}
			scaling := log2(mag)
			tileMap[TileSpec{scaling, plane}] = GeometryIndex(i)
			dvid.Infof("Plane %s at scaling %d set to geometry %d: resolution %s\n", plane, scaling, i, geom.PixelSize)
		}
	}

	// Initialize the googlevoxels data
	basedata, err := datastore.NewDataService(dtype, uuid, id, name, c)
	if err != nil {
		return nil, err
	}
	data := &Data{
		Data: basedata,
		Properties: Properties{
			VolumeID:     volumeid,
			AuthKey:      authkey,
			TileSize:     DefaultTileSize,
			TileMap:      tileMap,
			Scales:       m.Geoms,
			HighResIndex: highResIndex,
		},
	}
	return data, nil
}

// log2 returns the power of 2 necessary to cover the given value.
func log2(value float32) Scaling {
	var exp Scaling
	pow := float32(1.0)
	for {
		if pow >= value {
			return exp
		}
		pow *= 2
		exp++
	}
}

func (dtype *Type) Help() string {
	return HelpMessage
}

// TileSpec encapsulates the scale and orientation of a tile.
type TileSpec struct {
	scaling Scaling
	plane   TileOrientation
}

func (ts TileSpec) MarshalBinary() ([]byte, error) {
	return []byte{byte(ts.scaling), byte(ts.plane)}, nil
}

func (ts *TileSpec) UnmarshalBinary(data []byte) error {
	if len(data) != 2 {
		return fmt.Errorf("TileSpec serialization is 2 bytes.  Got %d bytes instead: %v", len(data), data)
	}
	ts.scaling = Scaling(data[0])
	ts.plane = TileOrientation(data[1])
	return nil
}

// GetTileSpec returns a TileSpec for a given scale and dvid Geometry.
func GetTileSpec(scaling Scaling, shape dvid.DataShape) (*TileSpec, error) {
	ts := new(TileSpec)
	ts.scaling = scaling
	switch {
	case shape.Equals(dvid.XY):
		ts.plane = XY
	case shape.Equals(dvid.XZ):
		ts.plane = XZ
	case shape.Equals(dvid.YZ):
		ts.plane = YZ
	default:
		return nil, fmt.Errorf("No Google BrainMaps slice orientation corresponding to DVID %s shape", shape)
	}
	return ts, nil
}

// Scaling describes the resolution where 0 is the highest resolution
type Scaling uint8

// TileOrientation describes the orientation of a tile.
type TileOrientation uint8

const (
	XY TileOrientation = iota
	XZ
	YZ
)

func (t TileOrientation) String() string {
	switch t {
	case XY:
		return "XY"
	case XZ:
		return "XZ"
	case YZ:
		return "YZ"
	default:
		return "Unknown orientation"
	}
}

// GeometryMap provides a mapping from DVID scale (0 is highest res) and tile orientation
// to the specific geometry (Google "scale" value) that supports it.
type GeometryMap map[TileSpec]GeometryIndex

func (gm GeometryMap) MarshalJSON() ([]byte, error) {
	s := "{"
	mapStr := make([]string, len(gm))
	i := 0
	for ts, gi := range gm {
		mapStr[i] = fmt.Sprintf(`"%s:%d": %d`, ts.plane, ts.scaling, gi)
		i++
	}
	s += strings.Join(mapStr, ",")
	s += "}"
	return []byte(s), nil
}

type GeometryIndex int

// Geometry corresponds to a Volume Geometry in Google BrainMaps API
type Geometry struct {
	VolumeSize   dvid.Point3d   `json:"volumeSize"`
	ChannelCount uint32         `json:"channelCount"`
	ChannelType  string         `json:"channelType"`
	PixelSize    dvid.NdFloat32 `json:"pixelSize"`
}

// JSON from Google API encodes unsigned long as string because javascript has limited max
// integers due to Javascript number types using double float.

type uint3d struct {
	X uint32
	Y uint32
	Z uint32
}

func (u *uint3d) UnmarshalJSON(b []byte) error {
	var m struct {
		X string `json:"x"`
		Y string `json:"y"`
		Z string `json:"z"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	x, err := strconv.Atoi(m.X)
	if err != nil {
		return fmt.Errorf("Could not parse X coordinate with unsigned long: %s", err.Error())
	}
	u.X = uint32(x)

	y, err := strconv.Atoi(m.Y)
	if err != nil {
		return fmt.Errorf("Could not parse Y coordinate with unsigned long: %s", err.Error())
	}
	u.Y = uint32(y)

	z, err := strconv.Atoi(m.Z)
	if err != nil {
		return fmt.Errorf("Could not parse Z coordinate with unsigned long: %s", err.Error())
	}
	u.Z = uint32(z)
	return nil
}

func (i uint3d) String() string {
	return fmt.Sprintf("%d x %d x %d", i.X, i.Y, i.Z)
}

type float3d struct {
	X float32 `json:"x"`
	Y float32 `json:"y"`
	Z float32 `json:"z"`
}

func (f float3d) String() string {
	return fmt.Sprintf("%f x %f x %f", f.X, f.Y, f.Z)
}

func (g *Geometry) UnmarshalJSON(b []byte) error {
	if g == nil {
		return fmt.Errorf("Can't unmarshal JSON into nil Geometry")
	}
	var m struct {
		VolumeSize   uint3d  `json:"volumeSize"`
		ChannelCount string  `json:"channelCount"`
		ChannelType  string  `json:"channelType"`
		PixelSize    float3d `json:"pixelSize"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	g.VolumeSize = dvid.Point3d{int32(m.VolumeSize.X), int32(m.VolumeSize.Y), int32(m.VolumeSize.Z)}
	g.PixelSize = dvid.NdFloat32{m.PixelSize.X, m.PixelSize.Y, m.PixelSize.Z}
	channels, err := strconv.Atoi(m.ChannelCount)
	if err != nil {
		return fmt.Errorf("Could not parse channelCount: %s", err.Error())
	}
	g.ChannelCount = uint32(channels)
	g.ChannelType = m.ChannelType
	return nil
}

type Geometries []Geometry

// GoogleTileSpec encapsulates all information needed for tile retrieval (aside from authentication)
// from the Google BrainMaps API, as well as processing the returned data.
type GoogleTileSpec struct {
	offset   dvid.Point3d
	size     dvid.Point3d // This is the size we can retrieve, not necessarily the requested size
	sizeWant dvid.Point3d // This is the requested size.
	gi       GeometryIndex
	edge     bool // Is the tile on the edge, i.e., partially outside a scaled volume?
	outside  bool // Is the tile totally outside any scaled volume?

	// cached data that immediately follows from the geometry index
	channelCount  uint32
	channelType   string
	bytesPerVoxel int32
}

// GetGoogleSpec returns a google-specific tile spec, which includes how the tile is positioned relative to
// scaled volume boundaries.  Not that the size parameter is the desired size and not what is required to fit
// within a scaled volume.
func (d *Data) GetGoogleSpec(scaling Scaling, plane dvid.DataShape, offset dvid.Point3d, size dvid.Point2d) (*GoogleTileSpec, error) {
	tile := new(GoogleTileSpec)
	tile.offset = offset

	// Convert combination of plane and size into 3d size.
	sizeWant, err := dvid.GetPoint3dFrom2d(plane, size, 1)
	if err != nil {
		return nil, err
	}
	tile.sizeWant = sizeWant

	// Determine which geometry is appropriate given the scaling and the shape/orientation
	tileSpec, err := GetTileSpec(scaling, plane)
	if err != nil {
		return nil, err
	}
	geomIndex, found := d.TileMap[*tileSpec]
	if !found {
		return nil, fmt.Errorf("Could not find scaled volume in %q for %s with scaling %d", d.DataName(), plane, scaling)
	}
	geom := d.Scales[geomIndex]
	tile.gi = geomIndex
	tile.channelCount = geom.ChannelCount
	tile.channelType = geom.ChannelType

	// Get the # bytes for each pixel
	switch geom.ChannelType {
	case "uint8":
		tile.bytesPerVoxel = 1
	case "float":
		tile.bytesPerVoxel = 4
	case "uint64":
		tile.bytesPerVoxel = 8
	default:
		return nil, fmt.Errorf("Unknown volume channel type in %s: %s", d.DataName(), geom.ChannelType)
	}

	// Check if the tile is completely outside the volume.
	volumeSize := geom.VolumeSize
	if offset[0] >= volumeSize[0] || offset[1] >= volumeSize[1] || offset[2] >= volumeSize[2] {
		tile.outside = true
		return tile, nil
	}

	// Check if the tile is on the edge and adjust size.
	var adjSize dvid.Point3d = sizeWant
	maxpt, err := offset.Expand2d(plane, size)
	for i := 0; i < 3; i++ {
		if maxpt[i] > volumeSize[i] {
			tile.edge = true
			adjSize[i] = volumeSize[i] - offset[i]
		}
	}
	tile.size = adjSize

	return tile, nil
}

// Returns the base API URL for retrieving an image tile.  Note that the authentication key
// or token needs to be added to the returned string to form a valid URL.  The formatStr
// parameter is of the form "jpeg" or "jpeg:80" or "png:8" where an optional compression
// level follows the image format and a colon.  Leave formatStr empty for default.
func (gts GoogleTileSpec) GetURL(volumeid, formatStr string) (string, error) {

	url := fmt.Sprintf("https://www.googleapis.com/brainmaps/v1beta1/volumes/%s:tile?", volumeid)
	url += fmt.Sprintf("corner=%d,%d,%d&", gts.offset[0], gts.offset[1], gts.offset[2])
	url += fmt.Sprintf("size=%d,%d,%d&", gts.size[0], gts.size[1], gts.size[2])
	url += fmt.Sprintf("scale=%d", gts.gi)

	if formatStr != "" {
		format := strings.Split(formatStr, ":")
		if format[0] == "jpg" {
			format[0] = "jpeg"
		}
		url += fmt.Sprintf("&format=%s", format[0])
		if len(format) > 1 {
			level, err := strconv.Atoi(format[1])
			if err != nil {
				return url, err
			}
			switch format[0] {
			case "jpeg":
				url += fmt.Sprintf("&jpegQuality=%d", level)
			case "png":
				url += fmt.Sprintf("&pngCompressionLevel=%d", level)
			}
		}
	}
	return url, nil
}

// padTile takes returned data and pads it to full tile size.
func (gts GoogleTileSpec) padTile(data []byte) ([]byte, error) {

	if gts.size[0]*gts.size[1]*gts.bytesPerVoxel != int32(len(data)) {
		return nil, fmt.Errorf("Before padding, for %d x %d x %d bytes/voxel tile, received %d bytes",
			gts.size[0], gts.size[1], gts.bytesPerVoxel, len(data))
	}

	inRowBytes := gts.size[0] * gts.bytesPerVoxel
	outRowBytes := gts.sizeWant[0] * gts.bytesPerVoxel
	outBytes := outRowBytes * gts.sizeWant[1]
	out := make([]byte, outBytes, outBytes)
	inI := int32(0)
	outI := int32(0)
	for y := int32(0); y < gts.size[1]; y++ {
		copy(out[outI:outI+inRowBytes], data[inI:inI+inRowBytes])
		inI += inRowBytes
		outI += outRowBytes
	}
	return out, nil
}

// Properties are additional properties for keyvalue data instances beyond those
// in standard datastore.Data.   These will be persisted to metadata storage.
type Properties struct {
	// Necessary information to select data from Google BrainMaps API.
	VolumeID string
	AuthKey  string

	// Default size in pixels along one dimension of square tile.
	TileSize int32

	// TileMap provides mapping between scale and tile orientation to Google scaling index.
	TileMap GeometryMap

	// Scales is the list of available precomputed scales ("geometries" in Google terms) for this data.
	Scales Geometries

	// HighResIndex is the geometry that is the highest resolution among the available scaled volumes.
	HighResIndex GeometryIndex
}

// MarshalJSON handles JSON serialization for googlevoxels Data.  It adds "Levels" metadata equivalent
// to multiscale2d's tile specification so clients can treat googlevoxels tile API identically to
// multiscale2d.  Sensitive information like AuthKey are withheld.
func (p Properties) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		VolumeID     string
		TileSize     int32
		TileMap      GeometryMap
		Scales       Geometries
		HighResIndex GeometryIndex
		Levels       multiscale2d.TileSpec
	}{
		p.VolumeID,
		p.TileSize,
		p.TileMap,
		p.Scales,
		p.HighResIndex,
		getTileSpec(p.TileSize, p.Scales[p.HighResIndex], p.TileMap),
	})
}

// Converts Google BrainMaps scaling to multiscale2d-style tile specifications.
// This assumes that Google levels always downsample by 2.
func getTileSpec(tileSize int32, hires Geometry, tileMap GeometryMap) multiscale2d.TileSpec {
	// Determine how many levels we have by the max of any orientation.
	// TODO -- Warn user in some way if BrainMaps API has levels in one orientation but not in other.
	var maxScale Scaling
	for tileSpec := range tileMap {
		if tileSpec.scaling > maxScale {
			maxScale = tileSpec.scaling
		}
	}

	// Create the levels from 0 (hires) to max level.
	levelSpec := multiscale2d.LevelSpec{
		TileSize: dvid.Point3d{tileSize, tileSize, tileSize},
	}
	levelSpec.Resolution = make(dvid.NdFloat32, 3)
	copy(levelSpec.Resolution, hires.PixelSize)
	ms2dTileSpec := make(multiscale2d.TileSpec, maxScale+1)
	for scale := Scaling(0); scale <= maxScale; scale++ {
		curSpec := levelSpec.Duplicate()
		ms2dTileSpec[multiscale2d.Scaling(scale)] = multiscale2d.TileScaleSpec{LevelSpec: curSpec}
		levelSpec.Resolution[0] *= 2
		levelSpec.Resolution[1] *= 2
		levelSpec.Resolution[2] *= 2
	}
	return ms2dTileSpec
}

// Data embeds the datastore's Data and extends it with voxel-specific properties.
type Data struct {
	*datastore.Data
	Properties
}

func (d *Data) GetVoxelSize(ts *TileSpec) (dvid.NdFloat32, error) {
	if d.Scales == nil || len(d.Scales) == 0 {
		return nil, fmt.Errorf("%s has no geometries and therefore no volumes for access", d.DataName())
	}
	if d.TileMap == nil {
		return nil, fmt.Errorf("%d has not been initialized and can't return voxel sizes", d.DataName())
	}
	if ts == nil {
		return nil, fmt.Errorf("Can't get voxel sizes for nil tile spec!")
	}
	scaleIndex := d.TileMap[*ts]
	if int(scaleIndex) > len(d.Scales) {
		return nil, fmt.Errorf("Can't map tile spec (%v) to available geometries", *ts)
	}
	geom := d.Scales[scaleIndex]
	return geom.PixelSize, nil
}

func (d *Data) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Base     *datastore.Data
		Extended Properties
	}{
		d.Data,
		d.Properties,
	})
}

func (d *Data) GobDecode(b []byte) error {
	buf := bytes.NewBuffer(b)
	dec := gob.NewDecoder(buf)
	if err := dec.Decode(&(d.Data)); err != nil {
		return err
	}
	if err := dec.Decode(&(d.Properties)); err != nil {
		return err
	}
	return nil
}

func (d *Data) GobEncode() ([]byte, error) {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(d.Data); err != nil {
		return nil, err
	}
	if err := enc.Encode(d.Properties); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// --- DataService interface ---

func (d *Data) Help() string {
	return HelpMessage
}

// Send transfers all key-value pairs pertinent to this data type as well as
// the storage.DataStoreType for them.
func (d *Data) Send(s message.Socket, roiname string, uuid dvid.UUID) error {
	dvid.Criticalf("googlevoxels.Send() is not implemented yet, so push/pull will not work for this data type.\n")
	return nil
}

// getBlankTileData returns a background 2d tile data
func (d *Data) getBlankTileImage(tile *GoogleTileSpec) (image.Image, error) {
	if tile == nil {
		return nil, fmt.Errorf("Can't get blank tile for unknown tile spec")
	}
	if d.Scales == nil || len(d.Scales) <= int(tile.gi) {
		return nil, fmt.Errorf("Scaled volumes for %d not suitable for tile spec", d.DataName())
	}

	// Generate the blank image
	numBytes := tile.sizeWant[0] * tile.sizeWant[1] * tile.bytesPerVoxel
	data := make([]byte, numBytes, numBytes)
	return dvid.GoImageFromData(data, int(tile.sizeWant[0]), int(tile.sizeWant[1]))
}

func (d *Data) serveTile(w http.ResponseWriter, r *http.Request, tile *GoogleTileSpec, formatStr string, noblanks bool) error {
	// If it's outside, write blank tile unless user wants no blanks.
	if tile.outside {
		if noblanks {
			http.NotFound(w, r)
			return fmt.Errorf("Requested tile is outside of available volume.")
		}
		img, err := d.getBlankTileImage(tile)
		if err != nil {
			return err
		}
		return dvid.WriteImageHttp(w, img, formatStr)
	}

	// If we are within volume, get data from Google.
	url, err := tile.GetURL(d.VolumeID, formatStr)
	if err != nil {
		return err
	}
	urlSansKey := url
	url += fmt.Sprintf("&key=%s", d.AuthKey)

	timedLog := dvid.NewTimeLog()
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	timedLog.Infof("PROXY HTTP to Google: %s, returned %d", urlSansKey, resp.StatusCode)
	defer resp.Body.Close()

	// Set the image header
	if err := dvid.SetImageHeader(w, formatStr); err != nil {
		return err
	}

	// If it's on edge, we need to pad the tile to the tile size.
	if tile.edge {
		// We need to read whole thing in to pad it.
		data, err := ioutil.ReadAll(resp.Body)
		dvid.Infof("Got edge tile from Google, %d bytes\n", len(data))
		if err != nil {
			return err
		}
		paddedData, err := tile.padTile(data)
		if err != nil {
			return err
		}
		_, err = w.Write(paddedData)
		return err
	}

	// If we aren't on edge or outside, our return status should be OK.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Unexpected status code %d on tile request (%q, volume id %q)", resp.StatusCode, d.DataName(), d.VolumeID)
	}

	// Just send the data as we get it from Google in chunks.
	respBytes := 0
	const BufferSize = 32 * 1024
	buf := make([]byte, BufferSize)
	for {
		n, err := resp.Body.Read(buf)
		respBytes += n
		eof := (err == io.EOF)
		if err != nil && !eof {
			return err
		}
		if _, err = w.Write(buf[:n]); err != nil {
			return err
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if eof {
			break
		}
	}
	dvid.Infof("Got non-edge tile from Google, %d bytes\n", respBytes)
	return nil
}

// ServeImage returns an image with appropriate Content-Type set.  This function differs
// from ServeTile in the way parameters are passed to it.  ServeTile accepts a tile coordinate.
// This function allows arbitrary offset and size, unconstrained by tile sizes.
func (d *Data) ServeImage(w http.ResponseWriter, r *http.Request, parts []string) error {
	if len(parts) < 7 {
		return fmt.Errorf("%q must be followed by shape/size/offset", parts[3])
	}
	shapeStr, sizeStr, offsetStr := parts[4], parts[5], parts[6]
	planeStr := dvid.DataShapeString(shapeStr)
	plane, err := planeStr.DataShape()
	if err != nil {
		return err
	}
	if plane.ShapeDimensions() != 2 {
		return fmt.Errorf("Quadtrees can only return 2d images not %s", plane)
	}

	size, err := dvid.StringToPoint2d(sizeStr, "_")
	if err != nil {
		return err
	}

	offset, err := dvid.StringToPoint3d(offsetStr, "_")
	if err != nil {
		return err
	}

	var formatStr string
	if len(parts) >= 8 {
		formatStr = parts[7]
	}
	if formatStr == "" {
		formatStr = DefaultTileFormat
	}

	// See if scaling was specified in query string, otherwise use high-res (scale 0)
	var scale Scaling
	queryValues := r.URL.Query()
	scalingStr := queryValues.Get("scale")
	if scalingStr != "" {
		scale64, err := strconv.ParseUint(scalingStr, 10, 8)
		if err != nil {
			return fmt.Errorf("Illegal tile scale: %s (%s)", scalingStr, err.Error())
		}
		scale = Scaling(scale64)
	}

	// Determine how this request sits in the available scaled volumes.
	googleTile, err := d.GetGoogleSpec(scale, plane, offset, size)
	if err != nil {
		return err
	}

	// Send the tile.
	return d.serveTile(w, r, googleTile, formatStr, true)
}

// ServeTile returns a tile with appropriate Content-Type set.
func (d *Data) ServeTile(w http.ResponseWriter, r *http.Request, parts []string) error {

	if len(parts) < 7 {
		return fmt.Errorf("'tile' request must be following by plane, scale level, and tile coordinate")
	}
	planeStr, scalingStr, coordStr := parts[4], parts[5], parts[6]
	queryValues := r.URL.Query()

	var noblanks bool
	noblanksStr := dvid.DataString(queryValues.Get("noblanks"))
	if noblanksStr == "true" {
		noblanks = true
	}

	var tilesize int32 = DefaultTileSize
	tileSizeStr := queryValues.Get("tilesize")
	if tileSizeStr != "" {
		tilesizeInt, err := strconv.Atoi(tileSizeStr)
		if err != nil {
			return err
		}
		tilesize = int32(tilesizeInt)
	}
	size := dvid.Point2d{tilesize, tilesize}

	var formatStr string
	if len(parts) >= 8 {
		formatStr = parts[7]
	}
	if formatStr == "" {
		formatStr = DefaultTileFormat
	}

	// Parse the tile specification
	plane := dvid.DataShapeString(planeStr)
	shape, err := plane.DataShape()
	if err != nil {
		err = fmt.Errorf("Illegal tile plane: %s (%s)", planeStr, err.Error())
		server.BadRequest(w, r, err.Error())
		return err
	}
	scale, err := strconv.ParseUint(scalingStr, 10, 8)
	if err != nil {
		err = fmt.Errorf("Illegal tile scale: %s (%s)", scalingStr, err.Error())
		server.BadRequest(w, r, err.Error())
		return err
	}
	tileCoord, err := dvid.StringToPoint(coordStr, "_")
	if err != nil {
		err = fmt.Errorf("Illegal tile coordinate: %s (%s)", coordStr, err.Error())
		server.BadRequest(w, r, err.Error())
		return err
	}

	// Convert tile coordinate to offset.
	var ox, oy, oz int32
	switch {
	case shape.Equals(dvid.XY):
		ox = tileCoord.Value(0) * tilesize
		oy = tileCoord.Value(1) * tilesize
		oz = tileCoord.Value(2)
	case shape.Equals(dvid.XZ):
		ox = tileCoord.Value(0) * tilesize
		oy = tileCoord.Value(1)
		oz = tileCoord.Value(2) * tilesize
	case shape.Equals(dvid.YZ):
		ox = tileCoord.Value(0)
		oy = tileCoord.Value(1) * tilesize
		oz = tileCoord.Value(2) * tilesize
	default:
		return fmt.Errorf("Unknown tile orientation: %s", shape)
	}

	// Determine how this request sits in the available scaled volumes.
	googleTile, err := d.GetGoogleSpec(Scaling(scale), shape, dvid.Point3d{ox, oy, oz}, size)
	if err != nil {
		server.BadRequest(w, r, err.Error())
		return err
	}

	// Send the tile.
	return d.serveTile(w, r, googleTile, formatStr, noblanks)
}

// DoRPC handles the 'generate' command.
func (d *Data) DoRPC(request datastore.Request, reply *datastore.Response) error {
	return fmt.Errorf("Unknown command.  Data instance %q does not support any commands.  See API help.")
}

// ServeHTTP handles all incoming HTTP requests for this data.
func (d *Data) ServeHTTP(requestCtx context.Context, w http.ResponseWriter, r *http.Request) {
	timedLog := dvid.NewTimeLog()

	action := strings.ToLower(r.Method)
	switch action {
	case "get":
		// Acceptable
	default:
		server.BadRequest(w, r, "googlevoxels can only handle GET HTTP verbs at this time")
		return
	}

	// Break URL request into arguments
	url := r.URL.Path[len(server.WebAPIPath):]
	parts := strings.Split(url, "/")
	if len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	if len(parts) < 4 {
		server.BadRequest(w, r, "incomplete API request")
		return
	}

	switch parts[3] {
	case "help":
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, d.Help())

	case "info":
		jsonBytes, err := d.MarshalJSON()
		if err != nil {
			server.BadRequest(w, r, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, string(jsonBytes))

	case "tile":
		if err := d.ServeTile(w, r, parts); err != nil {
			server.BadRequest(w, r, err.Error())
			return
		}
		timedLog.Infof("HTTP %s: tile (%s)", r.Method, r.URL)

	case "raw":
		if err := d.ServeImage(w, r, parts); err != nil {
			server.BadRequest(w, r, err.Error())
			return
		}
		timedLog.Infof("HTTP %s: image (%s)", r.Method, r.URL)
	default:
		server.BadRequest(w, r, "Illegal request for googlevoxels data.  See 'help' for REST API")
	}
}
